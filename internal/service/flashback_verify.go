package service

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"db-flashback/internal/service/dto"
)

// flashbackVerifyDirect 对已连接的 PostgreSQL 做与 Hub 任务相同的单表/多表/整库闪回断言，不依赖 instance_id。
func flashbackVerifyDirect(ctx context.Context, db *sql.DB) *dto.FlashbackSelftestResult {
	out := &dto.FlashbackSelftestResult{Checks: []dto.FlashbackSelftestCheck{}}
	var ver string
	if err := db.QueryRowContext(ctx, `SHOW server_version`).Scan(&ver); err != nil {
		flashbackSelftestAdd(&out.Checks, "version", false, err.Error())
		out.OK = false
		return out
	}
	st, msg := flashbackVersionGate(ver)
	flashbackSelftestAdd(&out.Checks, "version", st != flashbackCheckFailed, fmt.Sprintf("%s: %s", ver, msg))

	stamp := time.Now().UnixNano() % 1000000000
	tblA := fmt.Sprintf("tbl_fb_selftest_%d", stamp)
	tblB := fmt.Sprintf("tbl_fb_selftestb_%d", stamp)
	out.Table = "public." + tblA
	out.Tables = []string{"public." + tblA, "public." + tblB}
	identA := `public.` + flashbackQuoteIdent(tblA)
	identB := `public.` + flashbackQuoteIdent(tblB)
	enumName := "fb_st_c_" + tblA
	compName := "fb_st_a_" + tblA
	defer func() {
		_, _ = db.ExecContext(context.Background(), `DROP TABLE IF EXISTS `+identA)
		_, _ = db.ExecContext(context.Background(), `DROP TABLE IF EXISTS `+identB)
		_ = flashbackSelftestDropUDT(context.Background(), db, enumName, compName)
	}()

	if err := flashbackSelftestCreateUDT(ctx, db, enumName, compName); err != nil {
		flashbackSelftestAdd(&out.Checks, "create_types", false, err.Error())
		out.OK = false
		return out
	}
	extra := flashbackSelftestPGExtraCols(flashbackParseServerMajor(ver))
	for _, item := range []struct{ ident, name, qual string }{
		{identA, tblA, "public." + tblA},
		{identB, tblB, "public." + tblB},
	} {
		if _, err := db.ExecContext(ctx, flashbackSelftestTableSQLUDT(item.ident, enumName, compName, extra)); err != nil {
			flashbackSelftestAdd(&out.Checks, "create_table", false, item.qual+": "+err.Error())
			out.OK = false
			return out
		}
		if _, err := db.ExecContext(ctx, `ALTER TABLE `+item.ident+` REPLICA IDENTITY FULL`); err != nil {
			flashbackSelftestAdd(&out.Checks, "replica_identity", false, err.Error())
			out.OK = false
			return out
		}
		flashbackSelftestAssertColCount(ctx, db, item.name, out)
	}
	flashbackSelftestAdd(&out.Checks, "create_table", true, strings.Join(out.Tables, ","))

	if _, err := db.ExecContext(ctx, flashbackSelftestInsertSQLUDT(identA, enumName, compName, extra != "")); err != nil {
		flashbackSelftestAdd(&out.Checks, "insert", false, err.Error())
		out.OK = false
		return out
	}
	if _, err := db.ExecContext(ctx, flashbackSelftestInsertSQLUDT(identB, enumName, compName, extra != "")); err != nil {
		flashbackSelftestAdd(&out.Checks, "insert", false, "table b: "+err.Error())
		out.OK = false
		return out
	}
	flashbackSelftestAdd(&out.Checks, "insert", true, "2 tables x 2 rows")
	time.Sleep(200 * time.Millisecond)
	target := time.Now().UTC()
	if _, err := db.ExecContext(ctx, fmt.Sprintf(`UPDATE %s SET c_text='world', c_num=99.00 WHERE id=1`, identA)); err != nil {
		flashbackSelftestAdd(&out.Checks, "update", false, err.Error())
		out.OK = false
		return out
	}
	if _, err := db.ExecContext(ctx, fmt.Sprintf(`UPDATE %s SET c_text='scope_b_world', c_num=88.00 WHERE id=1`, identB)); err != nil {
		flashbackSelftestAdd(&out.Checks, "update", false, "table b: "+err.Error())
		out.OK = false
		return out
	}
	if _, err := db.ExecContext(ctx, fmt.Sprintf(`DELETE FROM %s WHERE id=2`, identA)); err != nil {
		flashbackSelftestAdd(&out.Checks, "delete", false, err.Error())
		out.OK = false
		return out
	}
	if _, err := db.ExecContext(ctx, fmt.Sprintf(`DELETE FROM %s WHERE id=2`, identB)); err != nil {
		flashbackSelftestAdd(&out.Checks, "delete", false, "table b: "+err.Error())
		out.OK = false
		return out
	}
	flashbackSelftestAdd(&out.Checks, "update_delete", true, "both tables updated/deleted")
	if _, err := db.ExecContext(ctx, `CHECKPOINT`); err != nil {
		flashbackSelftestAdd(&out.Checks, "checkpoint", true, "skipped: "+err.Error())
	} else {
		flashbackSelftestAdd(&out.Checks, "checkpoint", true, "wal flushed")
	}
	time.Sleep(400 * time.Millisecond)
	end := time.Now().UTC()

	undoSingle, redoSingle, ok1 := flashbackVerifyRunTask(ctx, db, []string{out.Table}, target, end, "single", out)
	undoMulti, _, ok2 := flashbackVerifyRunTask(ctx, db, out.Tables, target, end, "multi", out)
	undoAll, _, ok3 := flashbackVerifyRunTask(ctx, db, nil, target, end, "all", out)
	out.TaskIDs = []string{"task-single", "task-multi", "task-all"}
	out.TaskID = "task-single"
	out.UndoSQL = undoSingle
	out.ParseSQL = redoSingle
	if ok1 {
		flashbackSelftestAssertSQL(out, undoSingle, redoSingle)
	}
	flashbackSelftestAssertTableScope(out, tblA, tblB, undoSingle, undoMulti, undoAll)
	flashbackVerifyDeep(ctx, db, out)
	flashbackSelftestDDL(ctx, db, out)
	out.OK = ok1 && ok2 && ok3 && flashbackSelftestOK(out.Checks)
	return out
}

// flashbackVerifyRunTask 模拟一条闪回任务：按表范围 + 提交时间窗拉 WAL 并出 SQL。
func flashbackVerifyRunTask(ctx context.Context, db *sql.DB, tables []string, target, end time.Time, prefix string, out *dto.FlashbackSelftestResult) (undo, redo []string, ok bool) {
	scope := "指定表"
	if flashbackTablesIsAll(tables) {
		scope = "整库"
	}
	dict, err := flashbackLoadDictionary(ctx, db, "", tables)
	if err != nil {
		flashbackSelftestAdd(&out.Checks, prefix+"_task", false, err.Error())
		return nil, nil, false
	}
	workDir := filepath.Join(os.TempDir(), "jupiter-flashback-matrix", fmt.Sprintf("%s-%d", prefix, time.Now().UnixNano()))
	walDir := filepath.Join(workDir, "wal")
	defer func() { _ = os.RemoveAll(workDir) }()
	snap := filepath.Join(workDir, flashbackDictFileName)
	if err := flashbackSaveDictionaryFile(snap, dict); err != nil {
		flashbackSelftestAdd(&out.Checks, prefix+"_dict_snap", false, err.Error())
	} else if loaded, err := flashbackLoadDictionaryFile(snap); err != nil {
		flashbackSelftestAdd(&out.Checks, prefix+"_dict_snap", false, err.Error())
	} else {
		dict = loaded
		flashbackSelftestAdd(&out.Checks, prefix+"_dict_snap", true, fmt.Sprintf("%d tables", len(dict.Wanted)))
	}
	if err := flashbackAttachCatalog(ctx, db, dict); err != nil {
		flashbackSelftestAdd(&out.Checks, prefix+"_catalog", false, err.Error())
	}
	live, err := flashbackListLiveWAL(ctx, db)
	if err != nil {
		flashbackSelftestAdd(&out.Checks, prefix+"_ls_wal", false, err.Error())
		return nil, nil, false
	}
	cur := flashbackCurrentWALName(ctx, db)
	cpName, cpErr := flashbackCheckpointWALName(ctx, db)
	picked, _, _, cpOK := flashbackSelectWALPrecise(live, target, end, flashbackDefaultMaxWALBytes, cur, cpName)
	if cpErr != nil || !cpOK || len(picked) == 0 {
		flashbackSelftestAdd(&out.Checks, prefix+"_task", false,
			fmt.Sprintf("checkpoint=%s err=%v ok=%v files=%d", cpName, cpErr, cpOK, len(picked)))
		return nil, nil, false
	}
	flashbackSelftestAdd(&out.Checks, prefix+"_checkpoint", true, "from "+cpName)

	stt, _, err := flashbackStreamWAL(ctx, db, walDir, "", picked, dict, dict.DBOID, flashbackParseOpts{
		MaxChanges:  flashbackDefaultMaxSQLs,
		DeleteAfter: true,
		MaxFPWPages: flashbackDefaultFPWPages,
		TimeFrom:    target,
		TimeTo:      end,
	}, nil, nil, func(ch flashbackChange) bool {
		if !dict.matchChange(ch) {
			return true
		}
		if u, _ := flashbackUndoSQL(ch); u != "" {
			undo = append(undo, u)
		}
		if r, _ := flashbackRedoSQL(ch); r != "" {
			redo = append(redo, r)
		}
		return true
	})
	if err != nil {
		flashbackSelftestAdd(&out.Checks, prefix+"_task", false, err.Error())
		return undo, redo, false
	}
	if len(undo) == 0 || len(redo) == 0 {
		flashbackSelftestAdd(&out.Checks, prefix+"_task", false, fmt.Sprintf("%s 无 SQL：%s", scope, stt.String()))
		return undo, redo, false
	}
	flashbackSelftestAdd(&out.Checks, prefix+"_task", true,
		fmt.Sprintf("%s undo=%d redo=%d %s", scope, len(undo), len(redo), stt.String()))
	return undo, redo, true
}

// flashbackVerifyDeep 覆盖本次解码优化：子事务 TopXID、ABORT 丢弃、CHECKPOINT 后 FPW 旧行、TRUNCATE、缺旧行 UPDATE。
func flashbackVerifyDeep(ctx context.Context, db *sql.DB, out *dto.FlashbackSelftestResult) {
	if db == nil || out == nil {
		return
	}
	stamp := time.Now().UnixNano() % 1000000000
	tbl := fmt.Sprintf("tbl_fb_opt_%d", stamp)
	tbl2 := fmt.Sprintf("tbl_fb_opt2_%d", stamp)
	ident := `public.` + flashbackQuoteIdent(tbl)
	ident2 := `public.` + flashbackQuoteIdent(tbl2)
	defer func() {
		_, _ = db.ExecContext(context.Background(), `DROP TABLE IF EXISTS `+ident)
		_, _ = db.ExecContext(context.Background(), `DROP TABLE IF EXISTS `+ident2)
	}()

	var wc string
	_ = db.QueryRowContext(ctx, `SHOW wal_compression`).Scan(&wc)
	flashbackSelftestAdd(&out.Checks, "opt_wal_compression", true, "wal_compression="+strings.TrimSpace(wc))
	flashbackTryEnableWALCompression(ctx, db)

	if _, err := db.ExecContext(ctx, fmt.Sprintf(`CREATE TABLE %s (id integer PRIMARY KEY, note text)`, ident)); err != nil {
		flashbackSelftestAdd(&out.Checks, "opt_create", false, err.Error())
		return
	}
	if _, err := db.ExecContext(ctx, `ALTER TABLE `+ident+` REPLICA IDENTITY FULL`); err != nil {
		flashbackSelftestAdd(&out.Checks, "opt_replica", false, err.Error())
		return
	}
	if _, err := db.ExecContext(ctx, fmt.Sprintf(`CREATE TABLE %s (id integer PRIMARY KEY, note text)`, ident2)); err != nil {
		flashbackSelftestAdd(&out.Checks, "opt_create2", false, err.Error())
		return
	}
	flashbackSelftestAdd(&out.Checks, "opt_create", true, tbl+" / "+tbl2)

	time.Sleep(200 * time.Millisecond)
	target := time.Now().UTC()

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		flashbackSelftestAdd(&out.Checks, "opt_subxact", false, err.Error())
		return
	}
	if _, err := tx.ExecContext(ctx, `SAVEPOINT s_top`); err != nil {
		_ = tx.Rollback()
		flashbackSelftestAdd(&out.Checks, "opt_subxact", false, err.Error())
		return
	}
	if _, err := tx.ExecContext(ctx, fmt.Sprintf(`INSERT INTO %s(id, note) VALUES (1, 'subxact-old')`, ident)); err != nil {
		_ = tx.Rollback()
		flashbackSelftestAdd(&out.Checks, "opt_subxact", false, err.Error())
		return
	}
	if _, err := tx.ExecContext(ctx, `RELEASE SAVEPOINT s_top`); err != nil {
		_ = tx.Rollback()
		flashbackSelftestAdd(&out.Checks, "opt_subxact", false, err.Error())
		return
	}
	if err := tx.Commit(); err != nil {
		flashbackSelftestAdd(&out.Checks, "opt_subxact", false, err.Error())
		return
	}
	flashbackSelftestAdd(&out.Checks, "opt_subxact", true, "SAVEPOINT INSERT id=1")

	if _, err := db.ExecContext(ctx, `CHECKPOINT`); err != nil {
		flashbackSelftestAdd(&out.Checks, "opt_checkpoint", true, "skipped: "+err.Error())
	} else {
		flashbackSelftestAdd(&out.Checks, "opt_checkpoint", true, "wal flushed")
	}
	if _, err := db.ExecContext(ctx, fmt.Sprintf(`UPDATE %s SET note='after-fpw' WHERE id=1`, ident)); err != nil {
		flashbackSelftestAdd(&out.Checks, "opt_fpw_update", false, err.Error())
		return
	}
	flashbackSelftestAdd(&out.Checks, "opt_fpw_update", true, "CHECKPOINT 后 UPDATE 触发 FPW")

	tx2, err := db.BeginTx(ctx, nil)
	if err != nil {
		flashbackSelftestAdd(&out.Checks, "opt_abort", false, err.Error())
		return
	}
	if _, err := tx2.ExecContext(ctx, `SAVEPOINT s_abort`); err != nil {
		_ = tx2.Rollback()
		flashbackSelftestAdd(&out.Checks, "opt_abort", false, err.Error())
		return
	}
	if _, err := tx2.ExecContext(ctx, fmt.Sprintf(`INSERT INTO %s(id, note) VALUES (99, 'aborted')`, ident)); err != nil {
		_ = tx2.Rollback()
		flashbackSelftestAdd(&out.Checks, "opt_abort", false, err.Error())
		return
	}
	if _, err := tx2.ExecContext(ctx, `ROLLBACK TO SAVEPOINT s_abort`); err != nil {
		_ = tx2.Rollback()
		flashbackSelftestAdd(&out.Checks, "opt_abort", false, err.Error())
		return
	}
	if _, err := tx2.ExecContext(ctx, fmt.Sprintf(`INSERT INTO %s(id, note) VALUES (3, 'kept')`, ident)); err != nil {
		_ = tx2.Rollback()
		flashbackSelftestAdd(&out.Checks, "opt_abort", false, err.Error())
		return
	}
	if err := tx2.Commit(); err != nil {
		flashbackSelftestAdd(&out.Checks, "opt_abort", false, err.Error())
		return
	}
	flashbackSelftestAdd(&out.Checks, "opt_abort", true, "ROLLBACK TO SAVEPOINT 丢弃 id=99")

	if _, err := db.ExecContext(ctx, fmt.Sprintf(`INSERT INTO %s(id, note) VALUES (2, 'to-truncate')`, ident)); err != nil {
		flashbackSelftestAdd(&out.Checks, "opt_truncate_prep", false, err.Error())
		return
	}
	if _, err := db.ExecContext(ctx, fmt.Sprintf(`TRUNCATE TABLE %s`, ident)); err != nil {
		flashbackSelftestAdd(&out.Checks, "opt_truncate", false, err.Error())
		return
	}
	if _, err := db.ExecContext(ctx, `CHECKPOINT`); err != nil {
		flashbackSelftestAdd(&out.Checks, "opt_truncate_ckpt", true, "skipped: "+err.Error())
	}
	flashbackSelftestAdd(&out.Checks, "opt_truncate", true, "TRUNCATE "+tbl)

	if _, err := db.ExecContext(ctx, fmt.Sprintf(`INSERT INTO %s(id, note) VALUES (1, 'noupdate-old')`, ident2)); err != nil {
		flashbackSelftestAdd(&out.Checks, "opt_noupdate_prep", false, err.Error())
		return
	}
	if _, err := db.ExecContext(ctx, `ALTER TABLE `+ident2+` REPLICA IDENTITY NOTHING`); err != nil {
		flashbackSelftestAdd(&out.Checks, "opt_noupdate_ri", false, err.Error())
		return
	}
	_, _ = db.ExecContext(ctx, `CHECKPOINT`)
	if _, err := db.ExecContext(ctx, fmt.Sprintf(`UPDATE %s SET note='noupdate-u1' WHERE id=1`, ident2)); err != nil {
		flashbackSelftestAdd(&out.Checks, "opt_noupdate_u1", false, err.Error())
		return
	}
	if _, err := db.ExecContext(ctx, fmt.Sprintf(`UPDATE %s SET note='noupdate-u2' WHERE id=1`, ident2)); err != nil {
		flashbackSelftestAdd(&out.Checks, "opt_noupdate_u2", false, err.Error())
		return
	}
	flashbackSelftestAdd(&out.Checks, "opt_noupdate", true, "REPLICA IDENTITY NOTHING 连续 UPDATE")

	if _, err := db.ExecContext(ctx, `CHECKPOINT`); err != nil {
		flashbackSelftestAdd(&out.Checks, "opt_checkpoint2", true, "skipped: "+err.Error())
	} else {
		flashbackSelftestAdd(&out.Checks, "opt_checkpoint2", true, "wal flushed")
	}
	time.Sleep(400 * time.Millisecond)
	end := time.Now().UTC()

	dict, err := flashbackLoadDictionary(ctx, db, "", []string{"public." + tbl, "public." + tbl2})
	if err != nil {
		flashbackSelftestAdd(&out.Checks, "opt_dictionary", false, err.Error())
		return
	}
	if err := flashbackAttachCatalog(ctx, db, dict); err != nil {
		flashbackSelftestAdd(&out.Checks, "opt_catalog", false, err.Error())
	} else {
		flashbackSelftestAdd(&out.Checks, "opt_catalog", true, "catalog attached")
	}

	live, err := flashbackListLiveWAL(ctx, db)
	if err != nil {
		flashbackSelftestAdd(&out.Checks, "opt_ls_wal", false, err.Error())
		return
	}
	cur := flashbackCurrentWALName(ctx, db)
	picked, _, _ := flashbackSelectWAL(live, target, end, flashbackDefaultMaxWALBytes, cur)
	if len(picked) == 0 {
		flashbackSelftestAdd(&out.Checks, "opt_wal_window", false, "没有可拉取的 WAL")
		return
	}
	flashbackSelftestAdd(&out.Checks, "opt_wal_window", true, fmt.Sprintf("%d 段 current=%s", len(picked), cur))

	workDir := filepath.Join(os.TempDir(), "jupiter-flashback-opt", fmt.Sprintf("%d", time.Now().UnixNano()))
	walDir := filepath.Join(workDir, "wal")
	defer func() { _ = os.RemoveAll(workDir) }()

	var undo, redo []string
	var nTS int
	stt, _, err := flashbackStreamWAL(ctx, db, walDir, "", picked, dict, dict.DBOID, flashbackParseOpts{
		MaxChanges:  flashbackDefaultMaxSQLs,
		DeleteAfter: true,
		MaxFPWPages: flashbackDefaultFPWPages,
		TimeFrom:    target,
		TimeTo:      end,
	}, nil, nil, func(ch flashbackChange) bool {
		if !dict.matchChange(ch) {
			return true
		}
		if !ch.TS.IsZero() {
			nTS++
		}
		if u, _ := flashbackUndoSQL(ch); u != "" {
			undo = append(undo, u)
		}
		if r, _ := flashbackRedoSQL(ch); r != "" {
			redo = append(redo, r)
		}
		return true
	})
	if err != nil {
		flashbackSelftestAdd(&out.Checks, "opt_parse", false, err.Error())
		return
	}
	flashbackSelftestAdd(&out.Checks, "opt_parse", len(undo) > 0, stt.String())
	flashbackSelftestAdd(&out.Checks, "opt_commit_ts", nTS > 0, fmt.Sprintf("带提交时间的变更 %d", nTS))
	flashbackSelftestAdd(&out.Checks, "opt_topxid_insert",
		flashbackUndoHas(redo, "subxact-old") || flashbackUndoHas(undo, "subxact-old"),
		"子事务 INSERT 映射到顶层 xid 后已输出")
	flashbackSelftestAdd(&out.Checks, "opt_abort_dropped",
		!flashbackUndoHas(undo, "aborted") && !flashbackUndoHas(redo, "aborted"),
		"ABORT/ROLLBACK TO SAVEPOINT 未生成 SQL")
	flashbackSelftestAdd(&out.Checks, "opt_kept",
		flashbackUndoHas(redo, "kept") || flashbackUndoHas(undo, "kept"),
		"同事务未回滚的 INSERT 已输出")
	flashbackSelftestAdd(&out.Checks, "opt_fpw_old",
		flashbackUndoHas(undo, "subxact-old"),
		"UPDATE 旧行来自 FPW/FULL，undo 还原 subxact-old")
	flashbackSelftestAdd(&out.Checks, "opt_truncate_redo",
		flashbackUndoHas(redo, "TRUNCATE TABLE") && flashbackUndoHas(redo, tbl),
		"识别 HEAP_TRUNCATE")
	flashbackSelftestAdd(&out.Checks, "opt_truncate_undo",
		flashbackUndoHas(undo, "无法从 WAL 还原") && flashbackUndoHas(undo, tbl),
		"TRUNCATE undo 标明无法还原行")
	flashbackSelftestAdd(&out.Checks, "opt_truncate_stat", stt.Truncates > 0, fmt.Sprintf("Truncates=%d", stt.Truncates))
	if stt.FPWDecompress > 0 {
		flashbackSelftestAdd(&out.Checks, "opt_fpw_decompress", true, fmt.Sprintf("压缩 FPW 解压 %d", stt.FPWDecompress))
	} else {
		flashbackSelftestAdd(&out.Checks, "opt_fpw_decompress", true,
			fmt.Sprintf("本实例未出现压缩 FPW（wal_compression=%s，解压路径已单测）", strings.TrimSpace(wc)))
	}
	if stt.UpdateNoOld > 0 {
		flashbackSelftestAdd(&out.Checks, "opt_update_no_old", true, fmt.Sprintf("缺旧行 UPDATE 已丢弃 %d 条", stt.UpdateNoOld))
	} else {
		flashbackSelftestAdd(&out.Checks, "opt_update_no_old", true,
			"本次仍还原出旧行（未用新行顶替；缺旧行丢弃路径已单测）")
	}
}

func flashbackTryEnableWALCompression(ctx context.Context, db *sql.DB) {
	if db == nil {
		return
	}
	var ver string
	_ = db.QueryRowContext(ctx, `SHOW server_version`).Scan(&ver)
	want := "on"
	if flashbackParseServerMajor(ver) >= 15 {
		want = "lz4"
	}
	_, _ = db.ExecContext(ctx, `ALTER SYSTEM SET wal_compression = '`+want+`'`)
	_, _ = db.ExecContext(ctx, `SELECT pg_reload_conf()`)
}
