package service

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"db-flashback/internal/storage/flashback"
)

func (s *FlashbackImpl) executeTaskPDU(ctx context.Context, taskID string, row *flashback.TaskRow) error {
	ex := flashbackPDUExtraFromRow(row)
	s.logf(ctx, taskID, "INFO", "PDU 离线任务 scene=%s 时间窗 %s ~ %s（参考 PDU-PostgreSQLDataUnloader）",
		ex.Scene, row.TargetTime.Format(time.RFC3339), flashbackPDUEndTime(row).Format(time.RFC3339))
	srcPG, srcWAL := ex.PGDataPath, ex.ArchiveDest
	pgdata, walCopy, err := flashbackPDUPrepareWorkCopy(ctx, row.InstanceID, srcPG, srcWAL)
	if err != nil {
		return err
	}
	ex.PGDataPath = pgdata
	if walCopy != "" {
		ex.ArchiveDest = walCopy
	}
	s.logf(ctx, taskID, "INFO", "已在服务目录创建工作副本 PGDATA %s ← %s WAL %s", pgdata, srcPG, ex.ArchiveDest)
	s.writeFlashbackProgress(ctx, taskID, filepath.Join(flashbackWorkDirBase(ctx), taskID), 0, 0, 0, 0, 1, 0, 1)
	cat, err := flashbackOpenOfflinePGDATA(pgdata)
	if err != nil {
		return err
	}
	if err := cat.useDatabase(row.DatabaseName); err != nil {
		return err
	}
	var tables []string
	_ = json.Unmarshal([]byte(row.Tables), &tables)
	dict, err := cat.loadDictionary(tables)
	if err != nil && ex.Scene != flashbackPDUSceneDrop {
		return err
	}
	if dict != nil {
		_ = flashbackSaveDictionaryFile(flashbackDictPath(ctx, taskID), dict)
		s.logf(ctx, taskID, "INFO", "离线字典已加载：%d 张表，PG %s dboid=%d", len(dict.Wanted), cat.Version, dict.DBOID)
	}
	s.writeFlashbackStage(ctx, taskID, 1, 1, 0, 1)
	switch ex.Scene {
	case flashbackPDUSceneUnload:
		return s.flashbackPDURunUnload(ctx, taskID, row, cat, dict, ex)
	case flashbackPDUSceneDrop:
		return s.flashbackPDURunDrop(ctx, taskID, row, cat, dict, ex)
	default:
		return s.flashbackPDURunWAL(ctx, taskID, row, dict, ex)
	}
}

func flashbackPDUEndTime(row *flashback.TaskRow) time.Time {
	if row != nil && row.EndTime != nil {
		return *row.EndTime
	}
	return time.Now()
}

func (s *FlashbackImpl) flashbackPDURunUnload(ctx context.Context, taskID string, row *flashback.TaskRow, cat *flashbackOfflineCatalog, dict *flashbackDictionary, ex flashbackPDUExtra) error {
	if dict == nil {
		return fmt.Errorf("离线字典为空")
	}
	outDir := filepath.Join(flashbackWorkDirBase(ctx), taskID, "restore")
	if err := os.MkdirAll(outDir, 0o700); err != nil {
		return err
	}
	rels := make([]*flashbackRelation, 0, len(dict.Wanted))
	for _, rel := range dict.Wanted {
		if rel != nil && !rel.Missing {
			rels = append(rels, rel)
		}
	}
	s.writeFlashbackStage(ctx, taskID, 0, len(rels), 0, len(rels))
	var changeCount int
	for i, rel := range rels {
		path := flashbackHeapRelationPath(cat.PGData, cat.DBOID, rel.RelNode)
		s.logf(ctx, taskID, "INFO", "unload %s.%s ← %s", rel.Schema, rel.Name, path)
		tups, err := flashbackScanHeapFile(path, rel, ex.IncludeDead)
		if err != nil {
			s.logf(ctx, taskID, "WARN", "扫堆失败 %s.%s: %v", rel.Schema, rel.Name, err)
			continue
		}
		if rel.ToastNode != 0 {
			flashbackPDULoadToastFile(cat.PGData, cat.DBOID, rel, dict.Toast)
			for i := range tups {
				tups[i].Values = flashbackDecodeAttrs(rel, tups[i].Raw, tups[i].Infomask, flashbackSizeofHeapTuple, tups[i].Hoff)
			}
		}
		var rows []map[string]string
		for _, t := range tups {
			if t.Values != nil {
				rows = append(rows, t.Values)
			}
		}
		base := fmt.Sprintf("%s_%s", rel.Schema, rel.Name)
		csvPath := filepath.Join(outDir, base+".csv")
		n, err := flashbackPDUExportCSV(csvPath, rel, rows)
		if err != nil {
			return err
		}
		_ = s.flashbackSaveArtifact(ctx, taskID, "csv", csvPath, n)
		ddlPath := filepath.Join(outDir, base+".sql")
		if err := os.WriteFile(ddlPath, []byte(flashbackPDUExportDDL(rel)), 0o600); err == nil {
			_ = s.flashbackSaveArtifact(ctx, taskID, "ddl", ddlPath, 0)
		}
		copyPath := filepath.Join(outDir, base+"_copy.sql")
		if err := os.WriteFile(copyPath, []byte(flashbackPDUExportCOPY(rel, csvPath)), 0o600); err == nil {
			_ = s.flashbackSaveArtifact(ctx, taskID, "copy", copyPath, 0)
		}
		if ex.ExportMode == "sql" || ex.ExportMode == "both" {
			var stmts []string
			for _, r := range rows {
				if stmt := flashbackPDUInsertSQL(rel, r); stmt != "" {
					stmts = append(stmts, stmt)
				}
			}
			if len(stmts) > 0 {
				sqlPath := filepath.Join(outDir, base+"_insert.sql")
				if err := os.WriteFile(sqlPath, []byte(strings.Join(stmts, "\n")+"\n"), 0o600); err == nil {
					_ = s.flashbackSaveArtifact(ctx, taskID, "sql", sqlPath, len(stmts))
				}
				if err := s.flashbackPDUInsertSQLRows(ctx, taskID, rel, rows, &changeCount); err != nil {
					return err
				}
			}
		}
		changeCount += n
		s.writeFlashbackProgress(ctx, taskID, filepath.Join(flashbackWorkDirBase(ctx), taskID), 0, i+1, changeCount, i+1, len(rels), i+1, len(rels))
	}
	if strings.TrimSpace(ex.ArchiveDest) != "" {
		s.logf(ctx, taskID, "INFO", "unload 同时提供了 archive_dest，按时间窗追加 WAL 变更")
		_ = s.flashbackPDURunWAL(ctx, taskID, row, dict, ex)
	} else {
		s.logf(ctx, taskID, "INFO", "堆文件无提交时间，时间窗仅记录在任务上")
	}
	return s.store.UpdateStatus(ctx, taskID, flashback.StatusSucceeded, "", "")
}

func (s *FlashbackImpl) flashbackPDUInsertSQLRows(ctx context.Context, taskID string, rel *flashbackRelation, rows []map[string]string, seq *int) error {
	batch := make([]*flashback.SQLRow, 0, flashbackSQLInsertBatch)
	for _, row := range rows {
		stmt := flashbackPDUInsertSQL(rel, row)
		if stmt == "" {
			continue
		}
		*seq++
		batch = append(batch, &flashback.SQLRow{
			TaskID: taskID, Seq: *seq, Kind: flashback.KindUndo,
			SchemaName: rel.Schema, TableName: rel.Name, Op: "INSERT", Statement: stmt,
		})
		if len(batch) >= flashbackSQLInsertBatch {
			if err := s.store.InsertSQLs(ctx, batch); err != nil {
				return err
			}
			batch = batch[:0]
		}
	}
	if len(batch) > 0 {
		return s.store.InsertSQLs(ctx, batch)
	}
	return nil
}

func flashbackPDULoadToastFile(pgdata string, dboid uint32, rel *flashbackRelation, cache *flashbackToastCache) {
	if rel == nil || rel.ToastNode == 0 || cache == nil {
		return
	}
	toastRel := flashbackToastChunkRel()
	path := flashbackHeapRelationPath(pgdata, dboid, rel.ToastNode)
	tups, err := flashbackScanHeapFile(path, toastRel, true)
	if err != nil {
		return
	}
	for _, t := range tups {
		flashbackToastPutDecoded(cache, rel.ToastOID, t.Values)
	}
}

func (s *FlashbackImpl) flashbackPDURunWAL(ctx context.Context, taskID string, row *flashback.TaskRow, dict *flashbackDictionary, ex flashbackPDUExtra) error {
	if dict == nil {
		return fmt.Errorf("离线字典为空")
	}
	archive, err := flashbackPDUOpenDir(ctx, ex.ArchiveDest, "archive_dest")
	if err != nil {
		return err
	}
	files, err := flashbackListWorkWAL(archive)
	if err != nil {
		return err
	}
	files = flashbackPDUFilterWALNames(files, ex.StartWAL, ex.EndWAL)
	end := flashbackPDUEndTime(row)
	if len(files) == 0 {
		return flashbackPDUNoWALError(archive, files, row.TargetTime, end)
	}
	picked, bytes, truncated := flashbackSelectPDUWAL(files, flashbackMaxWALBytes(ctx))
	if len(picked) == 0 {
		return flashbackPDUNoWALError(archive, files, row.TargetTime, end)
	}
	if truncated {
		s.logf(ctx, taskID, "WARN", "WAL 体积超过上限，已从最旧段截断")
	}
	s.logf(ctx, taskID, "INFO", "按时间范围准备解析 %d 个本地 WAL（%d bytes）", len(picked), bytes)
	s.writeFlashbackProgress(ctx, taskID, filepath.Join(flashbackWorkDirBase(ctx), taskID), bytes, len(picked), 0, 0, len(picked), 0, len(picked))

	if ex.Scene == flashbackPDUSceneWALDelete && strings.TrimSpace(row.SQLType) == "" {
		row.SQLType = "delete"
	}
	if ex.Scene == flashbackPDUSceneWALUpdate && strings.TrimSpace(row.SQLType) == "" {
		row.SQLType = "update"
	}
	maxSQLs := flashbackMaxSQLs(ctx)
	opFilter := flashbackNormalizeSQLTypes(row.SQLType)
	seq := 1
	var changeCount int
	var flushErr error
	batch := make([]*flashback.SQLRow, 0, flashbackSQLInsertBatch)
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		if err := s.store.InsertSQLs(ctx, batch); err != nil {
			return err
		}
		batch = batch[:0]
		return nil
	}
	handle := func(ch flashbackChange) bool {
		if row.StartXID > 0 && int64(ch.XID) < row.StartXID {
			return true
		}
		if row.StopXID > 0 && int64(ch.XID) > row.StopXID {
			return true
		}
		if !ch.TS.IsZero() {
			if ch.TS.Before(row.TargetTime) || ch.TS.After(end) {
				return true
			}
		} else {
			return true
		}
		if !flashbackWantOp(opFilter, ch.Op) {
			return true
		}
		if !dict.matchChange(ch) {
			return true
		}
		if strings.EqualFold(ch.Op, "DELETE") || strings.EqualFold(ch.Op, "UPDATE") {
			if flashbackValuesIncomplete(dict.match(ch.Schema, ch.Table), ch.Old) {
				flashbackPDUFillOldFromHeap(ex.PGDataPath, dict, &ch)
			}
		}
		if strings.EqualFold(ch.Op, "DELETE") && flashbackValuesAllNull(ch.Old) {
			s.logf(ctx, taskID, "WARN", "DELETE %s.%s 未解出旧行（block=%d off=%d），已跳过", ch.Schema, ch.Table, ch.Block, ch.Offnum)
			return true
		}
		if changeCount >= maxSQLs {
			return false
		}
		undo, ur := flashbackUndoSQL(ch)
		redo, rr := flashbackRedoSQL(ch)
		var ts *time.Time
		if !ch.TS.IsZero() {
			t := ch.TS
			ts = &t
		}
		if undo != "" {
			batch = append(batch, &flashback.SQLRow{
				TaskID: taskID, Seq: seq, Kind: flashback.KindUndo,
				SchemaName: ch.Schema, TableName: ch.Table, Op: ch.Op,
				XID: int64(ch.XID), TS: ts, Statement: undo, Risk: ur,
			})
			seq++
			changeCount++
		}
		if redo != "" {
			batch = append(batch, &flashback.SQLRow{
				TaskID: taskID, Seq: seq, Kind: flashback.KindRedo,
				SchemaName: ch.Schema, TableName: ch.Table, Op: ch.Op,
				XID: int64(ch.XID), TS: ts, Statement: redo, Risk: rr,
			})
			seq++
		}
		if len(batch) >= flashbackSQLInsertBatch {
			if err := flush(); err != nil {
				flushErr = err
				return false
			}
		}
		return changeCount < maxSQLs
	}

	workDir := filepath.Join(flashbackWorkDirBase(ctx), taskID)
	walDir := filepath.Join(workDir, "wal")
	_ = os.MkdirAll(walDir, 0o700)
	_, pulled, err := flashbackStreamWAL(ctx, nil, walDir, archive, picked, dict, dict.DBOID, flashbackParseOpts{
		MaxChanges:  maxSQLs,
		DeleteAfter: false,
		MaxFPWPages: flashbackMaxFPWPages(ctx),
		TimeFrom:    row.TargetTime,
		TimeTo:      end,
	}, func(done, total int) {
		s.writeFlashbackStage(ctx, taskID, done, total, 0, total)
	}, func(name string, n int64, done, total int, written int64) {
		s.writeFlashbackProgress(ctx, taskID, workDir, written, total, changeCount, done, total, done, total)
		s.logf(ctx, taskID, "INFO", "已解析 %s（%d/%d）", name, done, total)
	}, handle)
	if err != nil {
		return err
	}
	if flushErr != nil {
		return flushErr
	}
	if err := flush(); err != nil {
		return err
	}
	if ex.ExportMode == "csv" || ex.ExportMode == "both" {
		_ = s.flashbackPDUDumpSQLToCSV(ctx, taskID)
	}
	_ = s.store.UpdateProgress(ctx, taskID, workDir, pulled, len(picked), changeCount)
	s.logf(ctx, taskID, "INFO", "PDU WAL 闪回完成，生成 %d 条变更", changeCount)
	return s.store.UpdateStatus(ctx, taskID, flashback.StatusSucceeded, "", "")
}

func (s *FlashbackImpl) flashbackPDUDumpSQLToCSV(ctx context.Context, taskID string) error {
	rows, _, err := s.store.ListSQLs(ctx, taskID, flashback.KindUndo, nil, 0, 100000)
	if err != nil {
		return err
	}
	path := filepath.Join(flashbackWorkDirBase(ctx), taskID, "restore", "wal_changes.csv")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	var b strings.Builder
	b.WriteString("seq,op,schema,table,sql\n")
	for _, r := range rows {
		if r.Kind != flashback.KindUndo {
			continue
		}
		b.WriteString(fmt.Sprintf("%d,%s,%s,%s,%q\n", r.Seq, r.Op, r.SchemaName, r.TableName, r.Statement))
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		return err
	}
	return s.flashbackSaveArtifact(ctx, taskID, "csv", path, len(rows))
}
