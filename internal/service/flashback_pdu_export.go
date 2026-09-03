package service

import (
	"context"
	"encoding/csv"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"

	"db-flashback/internal/storage/flashback"
)

func flashbackPDUExportCSV(path string, rel *flashbackRelation, rows []map[string]string) (int, error) {
	if rel == nil {
		return 0, fmt.Errorf("nil relation")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return 0, err
	}
	f, err := os.Create(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	w := csv.NewWriter(f)
	headers := make([]string, 0, len(rel.Columns))
	for _, c := range rel.Columns {
		if c.Dropped {
			continue
		}
		headers = append(headers, c.Name)
	}
	if err := w.Write(headers); err != nil {
		return 0, err
	}
	n := 0
	for _, row := range rows {
		rec := make([]string, len(headers))
		for i, h := range headers {
			v := row[h]
			if v == `\N` {
				v = ""
			}
			if !utf8.ValidString(v) {
				v = strings.ToValidUTF8(v, "?")
			}
			rec[i] = v
		}
		if err := w.Write(rec); err != nil {
			return n, err
		}
		n++
	}
	w.Flush()
	return n, w.Error()
}

func flashbackPDUExportDDL(rel *flashbackRelation) string {
	if rel == nil {
		return ""
	}
	var b strings.Builder
	b.WriteString("CREATE TABLE " + flashbackQualified(rel.Schema, rel.Name) + " (\n")
	var live []flashbackColumn
	for _, c := range rel.Columns {
		if !c.Dropped {
			live = append(live, c)
		}
	}
	for i, c := range live {
		typ := c.TypeName
		if typ == "" {
			typ = "text"
		}
		line := "  " + flashbackQuoteIdent(c.Name) + " " + typ
		if c.NotNull {
			line += " NOT NULL"
		}
		if i+1 < len(live) {
			line += ","
		}
		b.WriteString(line + "\n")
	}
	if len(rel.PKCols) > 0 {
		b.WriteString(");\n-- PRIMARY KEY: " + strings.Join(rel.PKCols, ", ") + "\n")
	} else {
		b.WriteString(");\n")
	}
	return b.String()
}

func flashbackPDUExportCOPY(rel *flashbackRelation, csvRel string) string {
	if rel == nil {
		return ""
	}
	var cols []string
	for _, c := range rel.Columns {
		if !c.Dropped {
			cols = append(cols, flashbackQuoteIdent(c.Name))
		}
	}
	sort.SliceStable(cols, func(i, j int) bool { return i < j })
	return fmt.Sprintf("\\copy %s (%s) FROM '%s' WITH (FORMAT csv, HEADER true, NULL '')\n",
		flashbackQualified(rel.Schema, rel.Name), strings.Join(cols, ", "), csvRel)
}

func flashbackPDUInsertSQL(rel *flashbackRelation, row map[string]string) string {
	if rel == nil || len(row) == 0 {
		return ""
	}
	ch := flashbackChange{
		Schema: rel.Schema, Table: rel.Name, Op: "INSERT", New: row, PKCols: rel.PKCols,
	}
	stmt, _ := flashbackRedoSQL(ch)
	return stmt
}

func (s *FlashbackImpl) flashbackSaveArtifact(ctx context.Context, taskID, kind, absPath string, rows int) error {
	work := filepath.Join(flashbackWorkDirBase(ctx), taskID)
	rel, err := filepath.Rel(work, absPath)
	if err != nil {
		rel = filepath.Base(absPath)
	}
	st, _ := os.Stat(absPath)
	var n int64
	if st != nil {
		n = st.Size()
	}
	return s.store.InsertArtifact(ctx, &flashback.ArtifactRow{
		TaskID: taskID, Kind: kind, RelPath: filepath.ToSlash(rel), Bytes: n, RowCount: rows,
	})
}

func flashbackPDUSafeJoin(workDir, relpath string) (string, error) {
	raw := strings.TrimSpace(relpath)
	if raw == "" || strings.Contains(raw, "..") {
		return "", fmt.Errorf("非法路径")
	}
	relpath = filepath.Clean("/" + raw)
	relpath = strings.TrimPrefix(relpath, string(filepath.Separator))
	if relpath == "" || strings.HasPrefix(relpath, "..") {
		return "", fmt.Errorf("非法路径")
	}
	full := filepath.Join(workDir, relpath)
	rel, err := filepath.Rel(workDir, full)
	if err != nil || strings.HasPrefix(rel, "..") {
		return "", fmt.Errorf("非法路径")
	}
	return full, nil
}
