package service

// DROP 碎页扫描参考 PDU-PostgreSQLDataUnloader dropscan_fs.c
// https://github.com/wublabdubdub/PDU-PostgreSQLDataUnloader
// Licensed under Apache License 2.0

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"db-flashback/internal/storage/flashback"
)

func (s *FlashbackImpl) flashbackPDURunDrop(ctx context.Context, taskID string, row *flashback.TaskRow, cat *flashbackOfflineCatalog, dict *flashbackDictionary, ex flashbackPDUExtra) error {
	root := filepath.Clean(ex.DiskPath)
	if err := flashbackPDUPathAllowed(ctx, root); err != nil {
		return err
	}
	st, err := os.Stat(root)
	if err != nil {
		return err
	}
	excludes := flashbackPDUExcludeDirs(ex.PGDataExclude, ex.PGDataPath)
	var rels []*flashbackRelation
	if dict != nil {
		for _, rel := range dict.Wanted {
			if rel != nil {
				rels = append(rels, rel)
			}
		}
	}
	if len(rels) == 0 {
		return fmt.Errorf("DROP 扫描没有可用的表元数据，请指定仍能从 catalog 或坟场解析的表")
	}
	s.logf(ctx, taskID, "INFO", "dropscan %s（排除 %d 个 PGDATA 路径）", root, len(excludes))
	var files []string
	if st.IsDir() {
		_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
			if err != nil || info == nil || info.IsDir() {
				return nil
			}
			if flashbackPDUPathExcluded(path, excludes) {
				return nil
			}
			if info.Size() < flashbackHeapPageSize {
				return nil
			}
			files = append(files, path)
			return nil
		})
	} else {
		files = []string{root}
	}
	if len(files) == 0 {
		return fmt.Errorf("扫描路径下没有可读文件")
	}
	s.writeFlashbackStage(ctx, taskID, 0, len(files), 0, len(files))
	outDir := filepath.Join(flashbackWorkDirBase(ctx), taskID, "restore")
	_ = os.MkdirAll(outDir, 0o700)
	found := map[string][]map[string]string{}
	for i, path := range files {
		for _, rel := range rels {
			tups := flashbackPDUScanFileAsRel(path, rel)
			if len(tups) == 0 {
				continue
			}
			qual := rel.Schema + "." + rel.Name
			for _, t := range tups {
				if t.Values != nil {
					found[qual] = append(found[qual], t.Values)
				}
			}
		}
		s.writeFlashbackStage(ctx, taskID, i+1, len(files), i+1, len(files))
	}
	var total int
	for _, rel := range rels {
		qual := rel.Schema + "." + rel.Name
		rows := found[qual]
		if len(rows) == 0 {
			continue
		}
		csvPath := filepath.Join(outDir, strings.ReplaceAll(qual, ".", "_")+"_drop.csv")
		n, err := flashbackPDUExportCSV(csvPath, rel, rows)
		if err != nil {
			return err
		}
		_ = s.flashbackSaveArtifact(ctx, taskID, "csv", csvPath, n)
		if ex.ExportMode != "csv" {
			_ = s.flashbackPDUInsertSQLRows(ctx, taskID, rel, rows, &total)
		}
		total += n
		s.logf(ctx, taskID, "INFO", "dropscan %s 找回 %d 行", qual, n)
	}
	if total == 0 {
		s.logf(ctx, taskID, "WARN", "未找回任何行；覆盖写/trim/已 vacuum 且无 WAL 的行无法恢复")
	}
	return s.store.UpdateStatus(ctx, taskID, flashback.StatusSucceeded, "", "")
}

func flashbackPDUExcludeDirs(raw, pgdata string) []string {
	var out []string
	for _, p := range strings.Split(raw, ",") {
		p = filepath.Clean(strings.TrimSpace(p))
		if p != "" && p != "." {
			out = append(out, p)
		}
	}
	if pgdata = filepath.Clean(strings.TrimSpace(pgdata)); pgdata != "" {
		out = append(out, pgdata)
	}
	return out
}

func flashbackPDUPathExcluded(path string, roots []string) bool {
	for _, root := range roots {
		rel, err := filepath.Rel(root, path)
		if err == nil && (rel == "." || !strings.HasPrefix(rel, "..")) {
			return true
		}
	}
	return false
}

func flashbackPDUScanFileAsRel(path string, rel *flashbackRelation) []flashbackHeapTuple {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	buf := make([]byte, flashbackHeapPageSize)
	var out []flashbackHeapTuple
	for blk := uint32(0); ; blk++ {
		n, err := io.ReadFull(f, buf)
		if n < flashbackHeapPageSize {
			break
		}
		if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
			break
		}
		page := flashbackScanHeapPage(buf, blk, rel, true)
		ok := 0
		for _, t := range page {
			if len(t.Values) >= max(1, len(rel.Columns)/2) {
				ok++
			}
		}
		if ok == 0 {
			continue
		}
		out = append(out, page...)
		if err == io.EOF {
			break
		}
	}
	return out
}
