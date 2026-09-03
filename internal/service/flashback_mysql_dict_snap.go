package service

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const flashbackMySQLDictEngine = "mysql"

type flashbackMySQLDictSnap struct {
	Version   int                     `json:"version"`
	Engine    string                  `json:"engine"`
	DBName    string                  `json:"db_name"`
	Relations []flashbackMySQLRelSnap `json:"relations"`
}

type flashbackMySQLRelSnap struct {
	Schema  string                  `json:"schema"`
	Name    string                  `json:"name"`
	PKCols  []string                `json:"pk_cols"`
	NoPK    bool                    `json:"no_pk"`
	Missing bool                    `json:"missing,omitempty"`
	Columns []flashbackMySQLColSnap `json:"columns"`
}

type flashbackMySQLColSnap struct {
	Name       string `json:"name"`
	DataType   string `json:"data_type"`
	ColumnType string `json:"column_type"`
	Charset    string `json:"charset"`
}

func flashbackMySQLDictionaryToSnap(dict *flashbackMySQLDict) flashbackMySQLDictSnap {
	snap := flashbackMySQLDictSnap{
		Version: flashbackDictSnapVersion,
		Engine:  flashbackMySQLDictEngine,
		DBName:  dict.DBName,
	}
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
		s := flashbackMySQLRelSnap{
			Schema: rel.Schema, Name: rel.Name,
			PKCols: append([]string(nil), rel.PKCols...),
			NoPK:   rel.NoPK, Missing: rel.Missing,
		}
		for _, c := range rel.Columns {
			s.Columns = append(s.Columns, flashbackMySQLColSnap{
				Name: c.Name, DataType: c.DataType, ColumnType: c.ColumnType, Charset: c.Charset,
			})
		}
		snap.Relations = append(snap.Relations, s)
	}
	return snap
}

func flashbackMySQLDictionaryFromSnap(snap flashbackMySQLDictSnap) *flashbackMySQLDict {
	dict := &flashbackMySQLDict{DBName: snap.DBName, Wanted: map[string]*flashbackMySQLRel{}}
	for _, s := range snap.Relations {
		rel := &flashbackMySQLRel{
			Schema: s.Schema, Name: s.Name,
			PKCols: append([]string(nil), s.PKCols...),
			NoPK:   s.NoPK, Missing: s.Missing,
		}
		for _, c := range s.Columns {
			rel.Columns = append(rel.Columns, flashbackMySQLCol{
				Name: c.Name, DataType: c.DataType, ColumnType: c.ColumnType, Charset: c.Charset,
			})
		}
		if !rel.NoPK {
			rel.NoPK = len(rel.PKCols) == 0
		}
		dict.Wanted[strings.ToLower(rel.Schema+"."+rel.Name)] = rel
	}
	return dict
}

func flashbackSaveMySQLDictionaryFile(path string, dict *flashbackMySQLDict) error {
	if dict == nil {
		return fmt.Errorf("nil mysql dictionary")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(flashbackMySQLDictionaryToSnap(dict), "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func flashbackLoadMySQLDictionaryFile(path string) (*flashbackMySQLDict, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var snap flashbackMySQLDictSnap
	if err := json.Unmarshal(raw, &snap); err != nil {
		return nil, err
	}
	if !strings.EqualFold(strings.TrimSpace(snap.Engine), flashbackMySQLDictEngine) {
		return nil, fmt.Errorf("not a mysql dictionary snapshot")
	}
	if snap.Version != 0 && snap.Version != flashbackDictSnapVersion {
		return nil, fmt.Errorf("unsupported mysql dictionary snapshot version %d", snap.Version)
	}
	dict := flashbackMySQLDictionaryFromSnap(snap)
	if dict == nil || len(dict.Wanted) == 0 {
		return nil, fmt.Errorf("empty mysql dictionary snapshot")
	}
	return dict, nil
}

// flashbackOpenTaskMySQLDictionary 优先读任务创建时冻结的 information_schema 快照。
func flashbackOpenTaskMySQLDictionary(ctx context.Context, db *sql.DB, taskID, dbName string, tables []string) (*flashbackMySQLDict, string, error) {
	path := flashbackDictPath(ctx, taskID)
	if dict, err := flashbackLoadMySQLDictionaryFile(path); err == nil && dict != nil && len(dict.Wanted) > 0 {
		return dict, "snapshot", nil
	}
	dict, err := flashbackLoadMySQLDictionary(ctx, db, dbName, tables)
	if err != nil {
		return nil, "", err
	}
	if err := flashbackSaveMySQLDictionaryFile(path, dict); err != nil {
		return dict, "live", nil
	}
	return dict, "live", nil
}
