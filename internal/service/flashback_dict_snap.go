package service

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

const (
	flashbackDictFileName    = "dict.json"
	flashbackDictSnapVersion = 1
)

type flashbackDictSnap struct {
	Version   int                     `json:"version"`
	DBOID     uint32                  `json:"dboid"`
	DBName    string                  `json:"db_name"`
	Relations []flashbackRelationSnap `json:"relations"`
}

type flashbackRelationSnap struct {
	Schema    string                `json:"schema"`
	Name      string                `json:"name"`
	OID       uint32                `json:"oid"`
	RelNode   uint32                `json:"relfilenode"`
	ToastNode uint32                `json:"toast_node"`
	ToastOID  uint32                `json:"toast_oid"`
	DBOID     uint32                `json:"dboid"`
	ReplIdent string                `json:"replident"`
	PKCols    []string              `json:"pk_cols"`
	Columns   []flashbackColumnSnap `json:"columns"`
	Missing   bool                  `json:"missing,omitempty"`
}

type flashbackColumnSnap struct {
	Name         string            `json:"name"`
	AttNum       int               `json:"attnum"`
	TypeName     string            `json:"type_name"`
	TypeOID      uint32            `json:"type_oid"`
	Typlen       int               `json:"typlen"`
	Typalign     string            `json:"typalign"`
	TypType      string            `json:"typtype"`
	Typelem      uint32            `json:"typelem"`
	BaseName     string            `json:"base_name"`
	BaseOID      uint32            `json:"base_oid"`
	ElemName     string            `json:"elem_name"`
	ElemTyplen   int               `json:"elem_typlen"`
	ElemTypalign string            `json:"elem_typalign"`
	EnumLabels   map[string]string `json:"enum_labels,omitempty"`
	NotNull      bool              `json:"not_null"`
	IsPK         bool              `json:"is_pk"`
	Dropped      bool              `json:"dropped"`
	Default      string            `json:"default,omitempty"`
}

func flashbackDictPath(ctx context.Context, taskID string) string {
	return filepath.Join(flashbackWorkDirBase(ctx), strings.TrimSpace(taskID), flashbackDictFileName)
}

func flashbackSaveDictionaryFile(path string, dict *flashbackDictionary) error {
	if dict == nil {
		return fmt.Errorf("nil dictionary")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(flashbackDictionaryToSnap(dict), "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func flashbackLoadDictionaryFile(path string) (*flashbackDictionary, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var snap flashbackDictSnap
	if err := json.Unmarshal(raw, &snap); err != nil {
		return nil, err
	}
	if snap.Version != 0 && snap.Version != flashbackDictSnapVersion {
		return nil, fmt.Errorf("unsupported dictionary snapshot version %d", snap.Version)
	}
	return flashbackDictionaryFromSnap(snap), nil
}

func flashbackDictionaryToSnap(dict *flashbackDictionary) flashbackDictSnap {
	snap := flashbackDictSnap{Version: flashbackDictSnapVersion, DBOID: dict.DBOID, DBName: dict.DBName}
	for _, key := range flashbackSortedKeys(func() map[string]string {
		out := map[string]string{}
		for k := range dict.Wanted {
			out[k] = k
		}
		return out
	}()) {
		rel := dict.Wanted[key]
		if rel == nil {
			continue
		}
		s := flashbackRelationSnap{
			Schema: rel.Schema, Name: rel.Name, OID: rel.OID, RelNode: rel.RelNode,
			ToastNode: rel.ToastNode, ToastOID: rel.ToastOID, DBOID: rel.DBOID,
			ReplIdent: rel.ReplIdent, PKCols: append([]string(nil), rel.PKCols...), Missing: rel.Missing,
		}
		for _, c := range rel.Columns {
			cs := flashbackColumnSnap{
				Name: c.Name, AttNum: c.AttNum, TypeName: c.TypeName, TypeOID: c.TypeOID,
				Typlen: c.Typlen, Typalign: c.Typalign, TypType: c.TypType, Typelem: c.Typelem,
				BaseName: c.BaseName, BaseOID: c.BaseOID, ElemName: c.ElemName,
				ElemTyplen: c.ElemTyplen, ElemTypalign: c.ElemTypalign,
				NotNull: c.NotNull, IsPK: c.IsPK, Dropped: c.Dropped, Default: c.Default,
			}
			if len(c.EnumLabels) > 0 {
				cs.EnumLabels = map[string]string{}
				for oid, lab := range c.EnumLabels {
					cs.EnumLabels[strconv.FormatUint(uint64(oid), 10)] = lab
				}
			}
			s.Columns = append(s.Columns, cs)
		}
		snap.Relations = append(snap.Relations, s)
	}
	return snap
}

func flashbackDictionaryFromSnap(snap flashbackDictSnap) *flashbackDictionary {
	dict := &flashbackDictionary{
		DBOID:     snap.DBOID,
		DBName:    snap.DBName,
		ByRelNode: map[uint32]*flashbackRelation{},
		Wanted:    map[string]*flashbackRelation{},
	}
	for _, s := range snap.Relations {
		rel := &flashbackRelation{
			Schema: s.Schema, Name: s.Name, OID: s.OID, RelNode: s.RelNode,
			ToastNode: s.ToastNode, ToastOID: s.ToastOID, DBOID: s.DBOID,
			ReplIdent: s.ReplIdent, PKCols: append([]string(nil), s.PKCols...),
			Missing: s.Missing, colByNum: map[int]flashbackColumn{},
		}
		if rel.DBOID == 0 {
			rel.DBOID = snap.DBOID
		}
		for _, c := range s.Columns {
			col := flashbackColumn{
				Name: c.Name, AttNum: c.AttNum, TypeName: c.TypeName, TypeOID: c.TypeOID,
				Typlen: c.Typlen, Typalign: c.Typalign, TypType: c.TypType, Typelem: c.Typelem,
				BaseName: c.BaseName, BaseOID: c.BaseOID, ElemName: c.ElemName,
				ElemTyplen: c.ElemTyplen, ElemTypalign: c.ElemTypalign,
				NotNull: c.NotNull, IsPK: c.IsPK, Dropped: c.Dropped, Default: c.Default,
			}
			if len(c.EnumLabels) > 0 {
				col.EnumLabels = map[uint32]string{}
				for k, lab := range c.EnumLabels {
					oid, _ := strconv.ParseUint(k, 10, 32)
					col.EnumLabels[uint32(oid)] = lab
				}
			}
			rel.Columns = append(rel.Columns, col)
			rel.colByNum[col.AttNum] = col
		}
		key := strings.ToLower(rel.Schema + "." + rel.Name)
		dict.Wanted[key] = rel
	}
	flashbackBindDictionary(dict)
	return dict
}

// flashbackOpenTaskDictionary 优先读任务字典快照，没有则从目标库导出并落盘。
func flashbackOpenTaskDictionary(ctx context.Context, db *sql.DB, taskID, dbName string, tables []string) (*flashbackDictionary, string, error) {
	path := flashbackDictPath(ctx, taskID)
	if dict, err := flashbackLoadDictionaryFile(path); err == nil && dict != nil && len(dict.Wanted) > 0 {
		return dict, "snapshot", nil
	}
	dict, err := flashbackLoadDictionary(ctx, db, dbName, tables)
	if err != nil {
		return nil, "", err
	}
	if err := flashbackSaveDictionaryFile(path, dict); err != nil {
		return dict, "live", nil
	}
	return dict, "live", nil
}

func flashbackCopyDictionaryFile(ctx context.Context, fromTaskID, toTaskID string) error {
	src := flashbackDictPath(ctx, strings.TrimSpace(fromTaskID))
	dst := flashbackDictPath(ctx, strings.TrimSpace(toTaskID))
	raw, err := os.ReadFile(src)
	if err != nil {
		return fmt.Errorf("加载源任务字典 %s: %w", fromTaskID, err)
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return err
	}
	tmp := dst + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, dst)
}
