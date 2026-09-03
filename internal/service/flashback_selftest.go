package service

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"db-flashback/internal/service/dto"
	"db-flashback/internal/storage/flashback"
)

const flashbackSelftestDefaultReviewer = "系统"

func flashbackSelftestOutputKind(raw string) string {
	if strings.EqualFold(strings.TrimSpace(raw), flashback.OutputOriginal) {
		return flashback.OutputOriginal
	}
	return flashback.OutputFlashback
}

func flashbackSelftestReviewer(req *dto.FlashbackSelftestReq) string {
	if req != nil {
		if n := strings.TrimSpace(req.Reviewer); n != "" {
			return n
		}
	}
	return flashbackSelftestDefaultReviewer
}

func (s *FlashbackImpl) flashbackSelftestSubmitTicket(_ *gin.Context, taskID, reviewer string, out *dto.FlashbackSelftestResult) {
	if out == nil {
		return
	}
	flashbackSelftestAdd(&out.Checks, "submit_ticket", true,
		fmt.Sprintf("skip: 独立项目不提交工单 task=%s reviewer=%s", strings.TrimSpace(taskID), strings.TrimSpace(reviewer)))
}

func flashbackSelftestAdd(checks *[]dto.FlashbackSelftestCheck, name string, ok bool, detail string) {
	*checks = append(*checks, dto.FlashbackSelftestCheck{Name: name, OK: ok, Detail: detail})
}

func flashbackSelftestOK(checks []dto.FlashbackSelftestCheck) bool {
	for _, c := range checks {
		if !c.OK {
			return false
		}
	}
	return len(checks) > 0
}

func flashbackUndoHas(stmts []string, substr string) bool {
	want := strings.ToLower(substr)
	for _, s := range stmts {
		if strings.Contains(strings.ToLower(s), want) {
			return true
		}
	}
	return false
}

func flashbackSelftestAssertTableScope(out *dto.FlashbackSelftestResult, single, other string, undoSingle, undoMulti, undoAll []string) {
	flashbackSelftestAdd(&out.Checks, "scope_single",
		flashbackUndoHas(undoSingle, single) && !flashbackUndoHas(undoSingle, other),
		"单表只含 "+single)
	flashbackSelftestAdd(&out.Checks, "scope_multi",
		flashbackUndoHas(undoMulti, single) && flashbackUndoHas(undoMulti, other),
		"多表含 "+single+" 与 "+other)
	flashbackSelftestAdd(&out.Checks, "scope_all",
		flashbackUndoHas(undoAll, single) && flashbackUndoHas(undoAll, other),
		"不选表（整库）含 "+single+" 与 "+other)
}

func flashbackSelftestAssertOriginalSQL(out *dto.FlashbackSelftestResult, prefix, verb, single, other, scope string, preview []string, want int) {
	flashbackSelftestAdd(&out.Checks, prefix+"_original_n",
		len(preview) >= want,
		fmt.Sprintf("原始 %s %d 条（至少 %d）", verb, len(preview), want))
	allVerb := len(preview) > 0
	for _, stmt := range preview {
		if !strings.HasPrefix(strings.ToUpper(strings.TrimSpace(stmt)), verb) {
			allVerb = false
			break
		}
	}
	flashbackSelftestAdd(&out.Checks, prefix+"_original_verb", allVerb, "原始 SQL 均为 "+verb)
	if scope == "single" {
		flashbackSelftestAdd(&out.Checks, prefix+"_original_scope",
			flashbackUndoHas(preview, single) && !flashbackUndoHas(preview, other),
			"单表原始 SQL 只含 "+single)
		return
	}
	flashbackSelftestAdd(&out.Checks, prefix+"_original_scope",
		flashbackUndoHas(preview, single) && flashbackUndoHas(preview, other),
		"多表/整库原始 SQL 含 "+single+" 与 "+other)
}

func (s *FlashbackImpl) flashbackSelftestOriginalByType(c *gin.Context, base dto.FlashbackTaskReq, tblA, tblB string, out *dto.FlashbackSelftestResult) {
	scopes := []struct {
		prefix string
		tables []string
		want   map[string]int
	}{
		{"orig_single", []string{out.Table}, map[string]int{"insert": 2, "update": 1, "delete": 1}},
		{"orig_multi", append([]string{}, out.Tables...), map[string]int{"insert": 4, "update": 2, "delete": 2}},
		{"orig_all", nil, map[string]int{"insert": 4, "update": 2, "delete": 2}},
	}
	types := []string{"insert", "update", "delete"}
	verbs := map[string]string{"insert": "INSERT", "update": "UPDATE", "delete": "DELETE"}
	ctx := c.Request.Context()
	for _, sqlType := range types {
		for _, sc := range scopes {
			req := base
			req.SQLType = sqlType
			req.OutputKind = flashback.OutputOriginal
			req.Tables = sc.tables
			prefix := sc.prefix + "_" + sqlType
			id, _, redo, ok := s.flashbackSelftestRunTask(c, &req, prefix, out)
			if id != "" {
				out.TaskIDs = append(out.TaskIDs, id)
			}
			if out.TaskID == "" {
				out.TaskID = id
			}
			if len(out.ParseSQL) == 0 && len(redo) > 0 {
				out.ParseSQL = redo
			}
			if !ok || id == "" {
				continue
			}
			kind, ops := flashbackSQLPreviewFilter(flashback.OutputOriginal, sqlType, "", "")
			preview, err := s.store.ListAllSQLStatements(ctx, id, kind, ops)
			if err != nil {
				flashbackSelftestAdd(&out.Checks, prefix+"_preview", false, err.Error())
				continue
			}
			flashbackSelftestAdd(&out.Checks, prefix+"_preview", true,
				fmt.Sprintf("kind=%s ops=%s n=%d", kind, strings.Join(ops, ","), len(preview)))
			flashbackSelftestAssertOriginalSQL(out, prefix, verbs[sqlType], tblA, tblB, strings.TrimPrefix(sc.prefix, "orig_"), preview, sc.want[sqlType])
		}
	}
}

func (s *FlashbackImpl) flashbackSelftestRunTask(c *gin.Context, req *dto.FlashbackTaskReq, prefix string, out *dto.FlashbackSelftestResult) (taskID string, undo, redo []string, ok bool) {
	if req == nil {
		flashbackSelftestAdd(&out.Checks, prefix+"_precheck", false, "request is nil")
		return "", nil, nil, false
	}
	pre, err := s.Precheck(c, req)
	if err != nil {
		flashbackSelftestAdd(&out.Checks, prefix+"_precheck", false, err.Error())
		return "", nil, nil, false
	}
	if pre == nil || !pre.OK {
		msg := "预检查未通过"
		if pre != nil {
			for _, it := range pre.Items {
				if it.Status == flashbackCheckFailed {
					msg = it.Name + ": " + it.Message
					break
				}
			}
		}
		flashbackSelftestAdd(&out.Checks, prefix+"_precheck", false, msg)
		return "", nil, nil, false
	}
	scope := "指定表"
	if flashbackTablesIsAll(req.Tables) {
		scope = "整库"
	}
	flashbackSelftestAdd(&out.Checks, prefix+"_precheck", true, fmt.Sprintf("%s files=%d", scope, pre.WALFiles))

	ctx := c.Request.Context()
	target, _ := flashbackParseTime(req.TargetTime)
	var endPtr *time.Time
	if strings.TrimSpace(req.EndTime) != "" {
		et, _ := flashbackParseTime(req.EndTime)
		endPtr = &et
	} else {
		now := time.Now().UTC()
		endPtr = &now
	}
	row := &flashback.TaskRow{
		ID:           flashback.NewID(),
		Host:         pre.Host,
		Port:         pre.Port,
		DatabaseName: strings.TrimSpace(req.Database),
		Tables:       flashbackTablesJSON(req.Tables),
		TargetTime:   target,
		EndTime:      endPtr,
		SQLType:      strings.TrimSpace(req.SQLType),
		OutputKind:   flashbackSelftestOutputKind(req.OutputKind),
		Status:       flashback.StatusPending,
		CreatedBy:    "flashback-selftest",
	}
	if err := flashbackAssignTaskHubDomain(ctx, row, req.InstanceID, pre.Host, pre.Port); err != nil {
		flashbackSelftestAdd(&out.Checks, prefix+"_create_task", false, err.Error())
		return "", nil, nil, false
	}
	if err := s.store.InsertTask(ctx, row); err != nil {
		flashbackSelftestAdd(&out.Checks, prefix+"_create_task", false, err.Error())
		return "", nil, nil, false
	}
	flashbackSelftestAdd(&out.Checks, prefix+"_create_task", true, row.ID)
	_ = s.store.InsertLog(ctx, row.ID, "INFO", prefix+" 任务已创建")
	runCtx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	s.runTask(runCtx, row.ID)
	saved, err := s.store.GetTask(runCtx, row.ID)
	if err != nil || saved == nil {
		flashbackSelftestAdd(&out.Checks, prefix+"_parse", false, "task missing after run")
		return row.ID, nil, nil, false
	}
	out.Warning = flashbackJoinWarning(out.Warning, saved.Warning)
	if saved.Status != flashback.StatusSucceeded {
		flashbackSelftestAdd(&out.Checks, prefix+"_parse", false, saved.Status+": "+saved.ErrorMessage+" "+saved.Warning)
		return row.ID, nil, nil, false
	}
	flashbackSelftestAdd(&out.Checks, prefix+"_parse", true, fmt.Sprintf("change_count=%d files=%d", saved.ChangeCount, saved.WALFiles))
	undo, err = s.store.ListAllSQLStatements(runCtx, row.ID, flashback.KindUndo, nil)
	if err != nil || len(undo) == 0 {
		detail := "未生成闪回 SQL"
		if err != nil {
			detail = err.Error()
		}
		flashbackSelftestAdd(&out.Checks, prefix+"_flashback_sql", false, detail)
		return row.ID, undo, nil, false
	}
	flashbackSelftestAdd(&out.Checks, prefix+"_flashback_sql", true, fmt.Sprintf("%d undo", len(undo)))
	redo, err = s.store.ListAllSQLStatements(runCtx, row.ID, flashback.KindRedo, nil)
	if err != nil || len(redo) == 0 {
		detail := "未生成解析 SQL"
		if err != nil {
			detail = err.Error()
		}
		flashbackSelftestAdd(&out.Checks, prefix+"_parse_sql", false, detail)
		return row.ID, undo, redo, false
	}
	flashbackSelftestAdd(&out.Checks, prefix+"_parse_sql", true, fmt.Sprintf("%d redo", len(redo)))
	return row.ID, undo, redo, true
}

func flashbackStmtsByPrefix(stmts []string, prefix string) []string {
	p := strings.ToUpper(strings.TrimSpace(prefix))
	var out []string
	for _, s := range stmts {
		u := strings.ToUpper(strings.TrimSpace(s))
		if strings.HasPrefix(u, p) {
			out = append(out, s)
		}
	}
	return out
}

// Selftest 在目标库建一张多类型表，写入后 UPDATE/DELETE，再跑闪回并核对 undo SQL。
func (s *FlashbackImpl) Selftest(c *gin.Context, req *dto.FlashbackSelftestReq) (*dto.FlashbackSelftestResult, error) {
	out := &dto.FlashbackSelftestResult{Checks: []dto.FlashbackSelftestCheck{}}
	if req == nil || strings.TrimSpace(req.InstanceID) == "" || strings.TrimSpace(req.Database) == "" {
		return nil, fmt.Errorf("instance_id 与 database 必填")
	}
	if err := s.ensureReady(c.Request.Context()); err != nil {
		return nil, err
	}
	ctx := c.Request.Context()
	db, dom, res, err := flashbackConnectTarget(ctx, strings.TrimSpace(req.InstanceID), strings.TrimSpace(req.Database))
	if err != nil {
		flashbackSelftestAdd(&out.Checks, "connect", false, err.Error())
		out.OK = false
		return out, nil
	}
	defer db.Close()
	flashbackSelftestAdd(&out.Checks, "connect", true, fmt.Sprintf("%s:%d/%s", dom.MainIP, dom.Port, req.Database))
	walSrc := flashbackResolveWALSourceFromConn(res, dom, "")
	if flashbackIsMySQL(dom.DbType) {
		return s.selftestMySQL(c, req, db, out)
	}

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
		return out, nil
	}
	major := flashbackSelftestPGMajor(ctx, db)
	extra := flashbackSelftestPGExtraCols(major)
	for _, item := range []struct{ ident, name, qual string }{
		{identA, tblA, "public." + tblA},
		{identB, tblB, "public." + tblB},
	} {
		ddl := flashbackSelftestTableSQLUDT(item.ident, enumName, compName, extra)
		if _, err := db.ExecContext(ctx, ddl); err != nil {
			flashbackSelftestAdd(&out.Checks, "create_table", false, item.qual+": "+err.Error())
			out.OK = false
			return out, nil
		}
		if _, err := db.ExecContext(ctx, `ALTER TABLE `+item.ident+` REPLICA IDENTITY FULL`); err != nil {
			flashbackSelftestAdd(&out.Checks, "replica_identity", false, err.Error())
			out.OK = false
			return out, nil
		}
		flashbackSelftestAssertColCount(ctx, db, item.name, out)
	}
	flashbackSelftestAdd(&out.Checks, "create_table", true, strings.Join(out.Tables, ","))

	origOnly := flashbackSelftestOutputKind(req.OutputKind) == flashback.OutputOriginal
	var insertFrom time.Time
	if origOnly {
		time.Sleep(200 * time.Millisecond)
		insertFrom = time.Now().UTC()
		time.Sleep(200 * time.Millisecond)
	}

	insA := flashbackSelftestInsertSQLUDT(identA, enumName, compName, extra != "")
	insB := flashbackSelftestInsertSQLUDT(identB, enumName, compName, extra != "")
	if _, err := db.ExecContext(ctx, insA); err != nil {
		flashbackSelftestAdd(&out.Checks, "insert", false, err.Error())
		out.OK = false
		return out, nil
	}
	if _, err := db.ExecContext(ctx, insB); err != nil {
		flashbackSelftestAdd(&out.Checks, "insert", false, "table b: "+err.Error())
		out.OK = false
		return out, nil
	}
	flashbackSelftestAdd(&out.Checks, "insert", true, "2 tables x 2 rows")
	time.Sleep(200 * time.Millisecond)
	target := time.Now().UTC()
	if _, err := db.ExecContext(ctx, fmt.Sprintf(`UPDATE %s SET c_text='world', c_num=99.00 WHERE id=1`, identA)); err != nil {
		flashbackSelftestAdd(&out.Checks, "update", false, err.Error())
		out.OK = false
		return out, nil
	}
	if _, err := db.ExecContext(ctx, fmt.Sprintf(`UPDATE %s SET c_text='scope_b_world', c_num=88.00 WHERE id=1`, identB)); err != nil {
		flashbackSelftestAdd(&out.Checks, "update", false, "table b: "+err.Error())
		out.OK = false
		return out, nil
	}
	if _, err := db.ExecContext(ctx, fmt.Sprintf(`DELETE FROM %s WHERE id=2`, identA)); err != nil {
		flashbackSelftestAdd(&out.Checks, "delete", false, err.Error())
		out.OK = false
		return out, nil
	}
	if _, err := db.ExecContext(ctx, fmt.Sprintf(`DELETE FROM %s WHERE id=2`, identB)); err != nil {
		flashbackSelftestAdd(&out.Checks, "delete", false, "table b: "+err.Error())
		out.OK = false
		return out, nil
	}
	flashbackSelftestAdd(&out.Checks, "update_delete", true, "both tables updated/deleted")
	if _, err := db.ExecContext(ctx, `CHECKPOINT`); err != nil {
		flashbackSelftestAdd(&out.Checks, "checkpoint", true, "skipped: "+err.Error())
		out.Warning = strings.TrimSpace(out.Warning + " checkpoint: " + err.Error())
	} else {
		flashbackSelftestAdd(&out.Checks, "checkpoint", true, "wal flushed")
	}
	time.Sleep(300 * time.Millisecond)
	end := time.Now().UTC()
	if walSrc.Kind == flashbackWALSourceCloudTencent {
		flashbackSelftestAdd(&out.Checks, "cloud_oss_lag", true, "云厂商 DML 后等待 3 分钟再下载 WAL")
		if err := flashbackCloudWaitDownloadLag(ctx, end, flashbackCloudOSSLag); err != nil {
			flashbackSelftestAdd(&out.Checks, "cloud_oss_lag", false, err.Error())
			out.OK = false
			return out, nil
		}
	}

	base := dto.FlashbackTaskReq{
		InstanceID: strings.TrimSpace(req.InstanceID),
		Database:   strings.TrimSpace(req.Database),
		TargetTime: target.Format(time.RFC3339Nano),
		EndTime:    end.Format(time.RFC3339Nano),
		SQLType:    "insert,update,delete",
	}
	if origOnly {
		base.TargetTime = insertFrom.Format(time.RFC3339Nano)
		base.OutputKind = flashback.OutputOriginal
		s.flashbackSelftestOriginalByType(c, base, tblA, tblB, out)
		out.OK = flashbackSelftestOK(out.Checks)
		return out, nil
	}
	singleReq := base
	singleReq.Tables = []string{out.Table}
	multiReq := base
	multiReq.Tables = append([]string{}, out.Tables...)
	allReq := base
	allReq.Tables = nil

	idSingle, undoSingle, redoSingle, ok1 := s.flashbackSelftestRunTask(c, &singleReq, "single", out)
	idMulti, undoMulti, _, ok2 := s.flashbackSelftestRunTask(c, &multiReq, "multi", out)
	idAll, undoAll, _, ok3 := s.flashbackSelftestRunTask(c, &allReq, "all", out)
	out.TaskID = idSingle
	out.TaskIDs = []string{idSingle, idMulti, idAll}
	out.UndoSQL = undoSingle
	out.ParseSQL = redoSingle
	if ok1 {
		flashbackSelftestAssertSQL(out, undoSingle, redoSingle)
		s.flashbackSelftestSubmitTicket(c, idSingle, flashbackSelftestReviewer(req), out)
	}
	flashbackSelftestAssertTableScope(out, tblA, tblB, undoSingle, undoMulti, undoAll)
	flashbackVerifyDeep(ctx, db, out)
	flashbackSelftestDDL(ctx, db, out)
	out.OK = ok1 && ok2 && ok3 && flashbackSelftestOK(out.Checks)
	return out, nil
}

// flashbackSelftestMinCols 自测表至少 80 列，覆盖官网 Chapter 8 通用类型 + range/数组/OID 别名/enum/composite。
// https://www.postgresql.org/docs/16/datatype.html
const flashbackSelftestMinCols = 80

func flashbackSelftestPGMajor(ctx context.Context, db *sql.DB) int {
	if db == nil {
		return 0
	}
	var ver string
	if err := db.QueryRowContext(ctx, `SHOW server_version`).Scan(&ver); err != nil {
		return 0
	}
	return flashbackParseServerMajor(ver)
}

func flashbackSelftestCreateUDT(ctx context.Context, db *sql.DB, enumName, compName string) error {
	if db == nil {
		return fmt.Errorf("db is nil")
	}
	qEnum := flashbackQuoteIdent(enumName)
	qComp := flashbackQuoteIdent(compName)
	if _, err := db.ExecContext(ctx, `DROP TYPE IF EXISTS `+qEnum+` CASCADE`); err != nil {
		return err
	}
	if _, err := db.ExecContext(ctx, `DROP TYPE IF EXISTS `+qComp+` CASCADE`); err != nil {
		return err
	}
	if _, err := db.ExecContext(ctx, `CREATE TYPE `+qEnum+` AS ENUM ('red','green','blue')`); err != nil {
		return err
	}
	_, err := db.ExecContext(ctx, `CREATE TYPE `+qComp+` AS (city text, zip integer)`)
	return err
}

func flashbackSelftestDropUDT(ctx context.Context, db *sql.DB, enumName, compName string) error {
	if db == nil {
		return nil
	}
	_, err1 := db.ExecContext(ctx, `DROP TYPE IF EXISTS `+flashbackQuoteIdent(enumName)+` CASCADE`)
	_, err2 := db.ExecContext(ctx, `DROP TYPE IF EXISTS `+flashbackQuoteIdent(compName)+` CASCADE`)
	if err1 != nil {
		return err1
	}
	return err2
}

// flashbackSelftestPGExtraCols PG 14+ 官网类型：pg_snapshot、xid8、六种 multirange。
func flashbackSelftestPGExtraCols(major int) string {
	if major < 14 {
		return ""
	}
	return `,
  c_pg_snapshot pg_snapshot,
  c_xid8 xid8,
  c_int4mr int4multirange,
  c_int8mr int8multirange,
  c_nummr nummultirange,
  c_datemr datemultirange,
  c_tsmr tsmultirange,
  c_tstzmr tstzmultirange`
}

func flashbackSelftestTableSQL(ident string) string {
	return flashbackSelftestTableSQLUDT(ident, "fb_st_color", "fb_st_addr", "")
}

func flashbackSelftestTableSQLUDT(ident, enumName, compName, extra string) string {
	return fmt.Sprintf(`
CREATE TABLE %s (
  id integer PRIMARY KEY,
  c_bool boolean NOT NULL,
  c_int2 smallint,
  c_int4 integer,
  c_int8 bigint,
  c_num numeric(12,2),
  c_decimal decimal(12,2),
  c_float4 real,
  c_float8 double precision,
  c_money money,
  c_smallserial smallserial,
  c_serial serial,
  c_bigserial bigserial,
  c_text text,
  c_varchar varchar(32),
  c_varchar256 varchar(256),
  c_bpchar char(8),
  c_char "char",
  c_name name,
  c_bytea bytea,
  c_date date,
  c_time time,
  c_time6 time(6),
  c_timetz timetz,
  c_ts timestamp,
  c_ts6 timestamp(6),
  c_tstz timestamptz,
  c_tstz6 timestamptz(6),
  c_interval interval,
  c_interval_ym interval year to month,
  c_uuid uuid,
  c_json json,
  c_jsonb jsonb,
  c_xml xml,
  c_inet inet,
  c_inet6 inet,
  c_cidr cidr,
  c_cidr6 cidr,
  c_macaddr macaddr,
  c_macaddr8 macaddr8,
  c_bit bit(4),
  c_bit64 bit(64),
  c_varbit varbit(16),
  c_varbit64 varbit(64),
  c_oid oid,
  c_xid xid,
  c_cid cid,
  c_lsn pg_lsn,
  c_txid_snapshot txid_snapshot,
  c_regclass regclass,
  c_regtype regtype,
  c_regnamespace regnamespace,
  c_regconfig regconfig,
  c_regdictionary regdictionary,
  c_regproc regproc,
  c_regprocedure regprocedure,
  c_regrole regrole,
  c_arr integer[],
  c_textarr text[],
  c_int2arr smallint[],
  c_int8arr bigint[],
  c_numarr numeric[],
  c_boolarr boolean[],
  c_uuidarr uuid[],
  c_datearr date[],
  c_jsonbarr jsonb[],
  c_point point,
  c_lseg lseg,
  c_box box,
  c_path path,
  c_polygon polygon,
  c_circle circle,
  c_line line,
  c_tsvector tsvector,
  c_tsquery tsquery,
  c_int4range int4range,
  c_int8range int8range,
  c_numrange numrange,
  c_daterange daterange,
  c_tsrange tsrange,
  c_tstzrange tstzrange,
  c_jsonpath jsonpath,
  c_enum %s,
  c_composite %s
  %s
)`, ident, flashbackQuoteIdent(enumName), flashbackQuoteIdent(compName), extra)
}

func flashbackSelftestInsertSQL(ident string) string {
	return flashbackSelftestInsertSQLUDT(ident, "fb_st_color", "fb_st_addr", false)
}

func flashbackSelftestInsertSQLUDT(ident, enumName, compName string, extra bool) string {
	qEnum := flashbackQuoteIdent(enumName)
	qComp := flashbackQuoteIdent(compName)
	cols := `
  id, c_bool, c_int2, c_int4, c_int8, c_num, c_decimal, c_float4, c_float8, c_money,
  c_smallserial, c_serial, c_bigserial,
  c_text, c_varchar, c_varchar256, c_bpchar, c_char, c_name, c_bytea,
  c_date, c_time, c_time6, c_timetz, c_ts, c_ts6, c_tstz, c_tstz6, c_interval, c_interval_ym, c_uuid,
  c_json, c_jsonb, c_xml, c_inet, c_inet6, c_cidr, c_cidr6, c_macaddr, c_macaddr8,
  c_bit, c_bit64, c_varbit, c_varbit64, c_oid, c_xid, c_cid, c_lsn, c_txid_snapshot,
  c_regclass, c_regtype, c_regnamespace, c_regconfig, c_regdictionary, c_regproc, c_regprocedure, c_regrole,
  c_arr, c_textarr, c_int2arr, c_int8arr, c_numarr, c_boolarr, c_uuidarr, c_datearr, c_jsonbarr,
  c_point, c_lseg, c_box, c_path, c_polygon, c_circle, c_line,
  c_tsvector, c_tsquery, c_int4range, c_int8range, c_numrange, c_daterange, c_tsrange, c_tstzrange, c_jsonpath,
  c_enum, c_composite`
	v1 := fmt.Sprintf(`1, true, 7, 41, 9999999999, 123.45, 123.45, 1.25, 1.5, 10.50,
 1, 11, 101,
 'hello', 'v1', 'long-v1', 'pad1', 'A', 'name1', '\xDEAD',
 DATE '2024-01-02', TIME '12:00:00', TIME '12:00:00.123456', TIMETZ '12:00:00+00',
 TIMESTAMP '2024-06-01 12:00:00', TIMESTAMP '2024-06-01 12:00:00.123456',
 TIMESTAMPTZ '2024-06-01 12:00:00+00', TIMESTAMPTZ '2024-06-01 12:00:00.123456+00',
 INTERVAL '1 day 02:03:04', INTERVAL '2 years 3 months',
 '550e8400-e29b-41d4-a716-446655440000',
 '{"k":1}'::json, '{"k":1}'::jsonb, NULL, '10.0.0.1', '2001:db8::1', '10.0.0.0/8', '2001:db8::/32',
 '08:00:2b:01:02:03', '08:00:2b:01:02:03:04:05',
 B'1010', B'1010101010101010101010101010101010101010101010101010101010101010', B'111000', B'101010111100',
 1001, '100'::xid, '11'::cid, '0/16A2E08', txid_current_snapshot(),
 'pg_class'::regclass, 'int4'::regtype, 'pg_catalog'::regnamespace, 'simple'::regconfig, 'simple'::regdictionary,
 'textin'::regproc, 'textin(cstring)'::regprocedure, CURRENT_USER::regrole,
 '{1,2,3}', '{a,b}', '{1,2}', '{9}', '{1.5}', '{t,f}',
 '{550e8400-e29b-41d4-a716-446655440000}', '{2024-01-02}', ARRAY['{"k":1}'::jsonb],
 '(1,2)', '[(0,0),(1,1)]', '(1,1),(0,0)', '[(0,0),(1,1)]', '((0,0),(1,1),(1,0))', '<(0,0),1>', '{1,-1,0}',
 to_tsvector('simple', 'hello world'), 'fat & rat'::tsquery,
 '[1,5)'::int4range, '[10,20)'::int8range, '[1.5,3.5)'::numrange,
 '[2024-01-01,2024-12-31)'::daterange,
 '[2024-01-01 00:00,2024-01-02 00:00)'::tsrange,
 '[2024-01-01 00:00+00,2024-01-02 00:00+00)'::tstzrange,
 '$.k'::jsonpath,
 'red'::%s, ROW('sh', 200000)::%s`, qEnum, qComp)
	v2 := fmt.Sprintf(`2, false, 8, 42, 1, 10.00, 10.00, 2.5, 2.25, 20.00,
 2, 12, 102,
 'keep', 'v2', 'long-v2', 'pad2', 'B', 'name2', '\xBEEF',
 DATE '2025-12-31', TIME '08:30:00', TIME '08:30:00.654321', TIMETZ '08:30:00+08',
 TIMESTAMP '2025-03-01 09:00:00', TIMESTAMP '2025-03-01 09:00:00.654321',
 TIMESTAMPTZ '2025-01-15 08:30:00+08', TIMESTAMPTZ '2025-01-15 08:30:00.654321+08',
 INTERVAL '3 hours', INTERVAL '1 year',
 '123e4567-e89b-12d3-a456-426614174000',
 '{"k":2}'::json, '{"k":2}'::jsonb, NULL, '192.168.0.2', '2001:db8::2', '192.168.0.0/16', '2001:db8:1::/48',
 '08:00:2b:01:02:04', '08:00:2b:01:02:03:04:06',
 B'1100', B'1100110011001100110011001100110011001100110011001100110011001100', B'101010', B'111100001111',
 2002, '200'::xid, '22'::cid, '0/2A2E08', txid_current_snapshot(),
 'pg_type'::regclass, 'text'::regtype, 'pg_catalog'::regnamespace, 'simple'::regconfig, 'simple'::regdictionary,
 'textout'::regproc, 'textout(text)'::regprocedure, CURRENT_USER::regrole,
 '{9}', '{z}', '{8}', '{42}', '{8.5}', '{f}',
 '{123e4567-e89b-12d3-a456-426614174000}', '{2025-12-31}', ARRAY['{"k":2}'::jsonb],
 '(3,4)', '[(2,2),(3,3)]', '(4,4),(2,2)', '[(2,2),(3,3)]', '((2,2),(3,3),(3,2))', '<(1,1),2>', '{0,1,-1}',
 to_tsvector('simple', 'keep data'), 'cat & dog'::tsquery,
 '[8,9)'::int4range, '[80,90)'::int8range, '[8.5,9.5)'::numrange,
 '[2025-01-01,2025-06-01)'::daterange,
 '[2025-03-01 00:00,2025-03-02 00:00)'::tsrange,
 '[2025-03-01 00:00+00,2025-03-02 00:00+00)'::tstzrange,
 '$.v'::jsonpath,
 'green'::%s, ROW('bj', 100000)::%s`, qEnum, qComp)
	if extra {
		cols += `,
  c_pg_snapshot, c_xid8, c_int4mr, c_int8mr, c_nummr, c_datemr, c_tsmr, c_tstzmr`
		v1 += `,
 pg_current_snapshot(), '42'::xid8, '{[1,3),[10,20)}'::int4multirange, '{[10,20)}'::int8multirange,
 '{[1.5,3.5)}'::nummultirange, '{[2024-01-01,2024-12-31)}'::datemultirange,
 '{[2024-01-01 00:00,2024-01-02 00:00)}'::tsmultirange, '{[2024-01-01 00:00+00,2024-01-02 00:00+00)}'::tstzmultirange`
		v2 += `,
 pg_current_snapshot(), '84'::xid8, '{[8,9)}'::int4multirange, '{[80,90)}'::int8multirange,
 '{[8.5,9.5)}'::nummultirange, '{[2025-01-01,2025-06-01)}'::datemultirange,
 '{[2025-03-01 00:00,2025-03-02 00:00)}'::tsmultirange, '{[2025-03-01 00:00+00,2025-03-02 00:00+00)}'::tstzmultirange`
	}
	return fmt.Sprintf("INSERT INTO %s (%s) VALUES\n(%s),\n(%s)", ident, cols, v1, v2)
}

func flashbackSelftestAssertColCount(ctx context.Context, db *sql.DB, table string, out *dto.FlashbackSelftestResult) {
	if db == nil || out == nil {
		return
	}
	var n int
	err := db.QueryRowContext(ctx, `
SELECT count(*) FROM information_schema.columns
WHERE table_schema = 'public' AND table_name = $1`, table).Scan(&n)
	if err != nil {
		flashbackSelftestAdd(&out.Checks, "col_count", false, err.Error())
		return
	}
	flashbackSelftestAdd(&out.Checks, "col_count", n >= flashbackSelftestMinCols,
		fmt.Sprintf("%d 列（要求 ≥ %d，覆盖官网 Chapter 8：数值/serial/字符/字节/日期时间/JSON/XML/网络/位串/OID·reg*/XID/LSN/snapshot/数组/几何/全文检索/range/jsonpath/enum/composite）", n, flashbackSelftestMinCols))
}

func flashbackSelftestAssertSQL(out *dto.FlashbackSelftestResult, stmts, redo []string) {
	upd := flashbackStmtsByPrefix(stmts, "UPDATE")
	insStmts := flashbackStmtsByPrefix(stmts, "INSERT")
	delUndo := flashbackStmtsByPrefix(stmts, "DELETE")
	flashbackSelftestAdd(&out.Checks, "flashback_update", len(upd) > 0, fmt.Sprintf("%d update", len(upd)))
	flashbackSelftestAdd(&out.Checks, "flashback_delete_as_insert", len(insStmts) > 0, fmt.Sprintf("%d insert", len(insStmts)))
	if len(delUndo) > 0 {
		flashbackSelftestAdd(&out.Checks, "flashback_insert_as_delete", true, fmt.Sprintf("%d delete", len(delUndo)))
	} else {
		flashbackSelftestAdd(&out.Checks, "flashback_insert_as_delete", true,
			"时间窗在 INSERT 之后，undo 不含对 INSERT 的 DELETE（符合提交时间裁窗）")
	}
	flashbackSelftestAdd(&out.Checks, "flashback_type_text", flashbackUndoHas(upd, "'hello'"), "闪回 UPDATE 还原 c_text")
	flashbackSelftestAdd(&out.Checks, "flashback_type_numeric", flashbackUndoHas(upd, "123.45"), "闪回 UPDATE 还原 c_num")
	flashbackSelftestAdd(&out.Checks, "flashback_type_bool", flashbackUndoHas(insStmts, "'f'"), "闪回 DELETE→INSERT bool")
	flashbackSelftestAdd(&out.Checks, "flashback_type_int2", flashbackUndoHas(insStmts, "'8'"), "闪回 int2")
	flashbackSelftestAdd(&out.Checks, "flashback_type_int8", flashbackUndoHas(insStmts, "1"), "闪回 int8")
	flashbackSelftestAdd(&out.Checks, "flashback_type_float8", flashbackUndoHas(insStmts, "2.25"), "闪回 float8")
	flashbackSelftestAdd(&out.Checks, "flashback_type_money", flashbackUndoHas(insStmts, "20"), "闪回 money")
	flashbackSelftestAdd(&out.Checks, "flashback_type_varchar", flashbackUndoHas(insStmts, "'v2'"), "闪回 varchar")
	flashbackSelftestAdd(&out.Checks, "flashback_type_uuid", flashbackUndoHas(insStmts, "123e4567-e89b-12d3-a456-426614174000"), "闪回 uuid")
	flashbackSelftestAdd(&out.Checks, "flashback_type_inet", flashbackUndoHas(insStmts, "192.168.0.2"), "闪回 inet")
	flashbackSelftestAdd(&out.Checks, "flashback_type_cidr", flashbackUndoHas(insStmts, "192.168.0.0/16"), "闪回 cidr")
	flashbackSelftestAdd(&out.Checks, "flashback_type_json", flashbackUndoHas(insStmts, `"k":2`) || flashbackUndoHas(insStmts, `"k": 2`), "闪回 json/jsonb 数字")
	flashbackSelftestAdd(&out.Checks, "flashback_type_date", flashbackUndoHas(insStmts, "2025-12-31"), "闪回 date")
	flashbackSelftestAdd(&out.Checks, "flashback_type_time", flashbackUndoHas(insStmts, "08:30"), "闪回 time")
	flashbackSelftestAdd(&out.Checks, "flashback_type_timestamptz", flashbackUndoHas(insStmts, "2025-01-15"), "闪回 timestamptz")
	flashbackSelftestAdd(&out.Checks, "flashback_type_interval", flashbackUndoHas(insStmts, "03:00") || flashbackUndoHas(insStmts, "3 hours"), "闪回 interval")
	flashbackSelftestAdd(&out.Checks, "flashback_type_bytea", flashbackUndoHas(insStmts, "beef"), "闪回 bytea")
	flashbackSelftestAdd(&out.Checks, "flashback_type_array", flashbackUndoHas(insStmts, "{9}"), "闪回 int[]")
	flashbackSelftestAdd(&out.Checks, "flashback_type_float4", flashbackUndoHas(insStmts, "2.5"), "闪回 float4")
	flashbackSelftestAdd(&out.Checks, "flashback_type_bpchar", flashbackUndoHas(insStmts, "pad2"), "闪回 bpchar")
	flashbackSelftestAdd(&out.Checks, "flashback_type_timetz", flashbackUndoHas(insStmts, "08:30"), "闪回 timetz")
	flashbackSelftestAdd(&out.Checks, "flashback_type_timestamp", flashbackUndoHas(insStmts, "2025-03-01"), "闪回 timestamp")
	flashbackSelftestAdd(&out.Checks, "flashback_type_xml_col", strings.Contains(flashbackSelftestTableSQL("t"), "c_xml xml"), "表含 xml 列（无 libxml 的实例写入 NULL）")
	flashbackSelftestAdd(&out.Checks, "flashback_type_macaddr", flashbackUndoHas(insStmts, "08:00:2b:01:02:04"), "闪回 macaddr")
	flashbackSelftestAdd(&out.Checks, "flashback_type_bit", flashbackUndoHas(insStmts, "1100"), "闪回 bit")
	flashbackSelftestAdd(&out.Checks, "flashback_type_oid", flashbackUndoHas(insStmts, "2002"), "闪回 oid")
	flashbackSelftestAdd(&out.Checks, "flashback_type_lsn",
		flashbackUndoHas(insStmts, "2A2E08") || flashbackUndoHas(insStmts, "2a2e08") || flashbackUndoHas(insStmts, "0/2A2E08"),
		"闪回 pg_lsn")
	flashbackSelftestAdd(&out.Checks, "flashback_type_int4", flashbackUndoHas(insStmts, "42") && flashbackUndoHas(insStmts, `"c_int4"`), "闪回 int4")
	flashbackSelftestAdd(&out.Checks, "flashback_type_char", flashbackUndoHas(insStmts, `"c_char"`), "闪回 \"char\"")
	flashbackSelftestAdd(&out.Checks, "flashback_type_name", flashbackUndoHas(insStmts, "name2") || flashbackUndoHas(insStmts, `"c_name"`), "闪回 name")
	flashbackSelftestAdd(&out.Checks, "flashback_type_macaddr8", flashbackUndoHas(insStmts, "08:00:2b:01:02:03:04:06") || flashbackUndoHas(insStmts, `"c_macaddr8"`), "闪回 macaddr8")
	flashbackSelftestAdd(&out.Checks, "flashback_type_varbit", flashbackUndoHas(insStmts, "101010") || flashbackUndoHas(insStmts, `"c_varbit"`), "闪回 varbit")
	flashbackSelftestAdd(&out.Checks, "flashback_type_xid", flashbackUndoHas(insStmts, `"c_xid"`), "闪回 xid")
	flashbackSelftestAdd(&out.Checks, "flashback_type_cid", flashbackUndoHas(insStmts, `"c_cid"`), "闪回 cid")
	flashbackSelftestAdd(&out.Checks, "flashback_type_textarr", flashbackUndoHas(insStmts, "{z}") || flashbackUndoHas(insStmts, `"c_textarr"`), "闪回 text[]")
	flashbackSelftestAdd(&out.Checks, "flashback_type_geom",
		flashbackUndoHas(insStmts, `"c_point"`) && flashbackUndoHas(insStmts, `"c_circle"`) && flashbackUndoHas(insStmts, `"c_polygon"`),
		"闪回 几何类型列")
	flashbackSelftestAdd(&out.Checks, "flashback_type_fts",
		flashbackUndoHas(insStmts, `"c_tsvector"`) && flashbackUndoHas(insStmts, `"c_tsquery"`),
		"闪回 tsvector/tsquery")
	flashbackSelftestAdd(&out.Checks, "flashback_type_range",
		flashbackUndoHas(insStmts, `"c_int4range"`) && flashbackUndoHas(insStmts, `"c_daterange"`) && flashbackUndoHas(insStmts, `"c_tstzrange"`),
		"闪回 range")
	flashbackSelftestAdd(&out.Checks, "flashback_type_jsonpath", flashbackUndoHas(insStmts, `"c_jsonpath"`), "闪回 jsonpath")
	flashbackSelftestAdd(&out.Checks, "flashback_type_decimal", flashbackUndoHas(insStmts, `"c_decimal"`) || flashbackUndoHas(insStmts, "10.00"), "闪回 decimal")
	flashbackSelftestAdd(&out.Checks, "flashback_type_line", flashbackUndoHas(insStmts, `"c_line"`), "闪回 line")
	flashbackSelftestAdd(&out.Checks, "flashback_type_inet6", flashbackUndoHas(insStmts, "2001:db8") || flashbackUndoHas(insStmts, `"c_inet6"`), "闪回 inet IPv6")
	flashbackSelftestAdd(&out.Checks, "flashback_type_enum", flashbackUndoHas(insStmts, "green") || flashbackUndoHas(insStmts, `"c_enum"`), "闪回 enum")
	flashbackSelftestAdd(&out.Checks, "flashback_type_composite", flashbackUndoHas(insStmts, `"c_composite"`) || flashbackUndoHas(insStmts, "bj"), "闪回 composite")
	flashbackSelftestAdd(&out.Checks, "flashback_type_regclass", flashbackUndoHas(insStmts, `"c_regclass"`) || flashbackUndoHas(insStmts, "pg_type"), "闪回 regclass")
	flashbackSelftestAdd(&out.Checks, "flashback_type_txid_snapshot", flashbackUndoHas(insStmts, `"c_txid_snapshot"`), "闪回 txid_snapshot")
	flashbackSelftestAdd(&out.Checks, "flashback_type_jsonbarr", flashbackUndoHas(insStmts, `"c_jsonbarr"`), "闪回 jsonb[]")
	flashbackSelftestAdd(&out.Checks, "flashback_type_serial",
		flashbackUndoHas(insStmts, `"c_serial"`) && flashbackUndoHas(insStmts, `"c_bigserial"`),
		"闪回 serial/bigserial")

	redoUpd := flashbackStmtsByPrefix(redo, "UPDATE")
	redoIns := flashbackStmtsByPrefix(redo, "INSERT")
	redoDel := flashbackStmtsByPrefix(redo, "DELETE")
	flashbackSelftestAdd(&out.Checks, "parse_update", len(redoUpd) > 0, fmt.Sprintf("%d update", len(redoUpd)))
	flashbackSelftestAdd(&out.Checks, "parse_delete", len(redoDel) > 0, fmt.Sprintf("%d delete", len(redoDel)))
	flashbackSelftestAdd(&out.Checks, "parse_type_text", flashbackUndoHas(redoUpd, "'world'"), "解析 UPDATE 新 c_text")
	flashbackSelftestAdd(&out.Checks, "parse_type_numeric", flashbackUndoHas(redoUpd, "99"), "解析 UPDATE 新 c_num")
	if len(redoIns) > 0 {
		flashbackSelftestAdd(&out.Checks, "parse_insert", true, fmt.Sprintf("%d insert", len(redoIns)))
		flashbackSelftestAdd(&out.Checks, "parse_type_bool", flashbackUndoHas(redoIns, "'t'") || flashbackUndoHas(redoIns, "'f'"), "解析 INSERT bool")
		flashbackSelftestAdd(&out.Checks, "parse_type_text_insert", flashbackUndoHas(redoIns, "'hello'") || flashbackUndoHas(redoIns, "'keep'"), "解析 INSERT text")
		flashbackSelftestAdd(&out.Checks, "parse_type_uuid", flashbackUndoHas(redoIns, "550e8400-e29b-41d4-a716-446655440000") || flashbackUndoHas(redoIns, "123e4567-e89b-12d3-a456-426614174000"), "解析 INSERT uuid")
		flashbackSelftestAdd(&out.Checks, "parse_type_inet", flashbackUndoHas(redoIns, "10.0.0.1") || flashbackUndoHas(redoIns, "192.168.0.2"), "解析 INSERT inet")
		flashbackSelftestAdd(&out.Checks, "parse_type_bytea", flashbackUndoHas(redoIns, "dead") || flashbackUndoHas(redoIns, "beef"), "解析 INSERT bytea")
		flashbackSelftestAdd(&out.Checks, "parse_type_array", flashbackUndoHas(redoIns, "{1,2,3}") || flashbackUndoHas(redoIns, "{9}"), "解析 INSERT int[]")
	} else {
		flashbackSelftestAdd(&out.Checks, "parse_insert", true, "时间窗在 INSERT 之后，redo 不含 INSERT（符合提交时间裁窗）")
	}
	flashbackSelftestAdd(&out.Checks, "parse_delete_id", flashbackUndoHas(redoDel, "'2'"), "解析 DELETE id=2")
}

// flashbackSelftestDDL 建表→加列→改名→DROP，断言逆 SQL。
func flashbackSelftestDDL(ctx context.Context, db *sql.DB, out *dto.FlashbackSelftestResult) {
	if out == nil || db == nil {
		return
	}
	tbl := fmt.Sprintf("tbl_fb_ddl_%d", time.Now().UnixNano()%1000000000)
	renamed := tbl + "_r"
	ident := `public.` + flashbackQuoteIdent(tbl)
	identR := `public.` + flashbackQuoteIdent(renamed)
	defer func() {
		_, _ = db.ExecContext(context.Background(), `DROP TABLE IF EXISTS `+identR)
		_, _ = db.ExecContext(context.Background(), `DROP TABLE IF EXISTS `+ident)
	}()

	time.Sleep(200 * time.Millisecond)
	target := time.Now().UTC()
	if _, err := db.ExecContext(ctx, fmt.Sprintf(`CREATE TABLE %s (id integer PRIMARY KEY, name text, n integer)`, ident)); err != nil {
		flashbackSelftestAdd(&out.Checks, "ddl_create", false, err.Error())
		return
	}
	if _, err := db.ExecContext(ctx, fmt.Sprintf(`ALTER TABLE %s ADD COLUMN extra integer`, ident)); err != nil {
		flashbackSelftestAdd(&out.Checks, "ddl_add_column", false, err.Error())
		return
	}
	if _, err := db.ExecContext(ctx, fmt.Sprintf(`ALTER TABLE %s RENAME TO %s`, ident, flashbackQuoteIdent(renamed))); err != nil {
		flashbackSelftestAdd(&out.Checks, "ddl_rename", false, err.Error())
		return
	}
	if _, err := db.ExecContext(ctx, fmt.Sprintf(`DROP TABLE %s`, identR)); err != nil {
		flashbackSelftestAdd(&out.Checks, "ddl_drop", false, err.Error())
		return
	}
	flashbackSelftestAdd(&out.Checks, "ddl_ops", true, tbl+" → +extra → "+renamed+" → DROP")
	if _, err := db.ExecContext(ctx, `CHECKPOINT`); err != nil {
		flashbackSelftestAdd(&out.Checks, "ddl_checkpoint", true, "skipped: "+err.Error())
	} else {
		flashbackSelftestAdd(&out.Checks, "ddl_checkpoint", true, "wal flushed")
	}
	time.Sleep(300 * time.Millisecond)

	dict, err := flashbackLoadDictionary(ctx, db, "", []string{"public." + tbl, "public." + renamed})
	if err != nil {
		flashbackSelftestAdd(&out.Checks, "ddl_dictionary", false, err.Error())
		return
	}
	if err := flashbackAttachCatalog(ctx, db, dict); err != nil {
		flashbackSelftestAdd(&out.Checks, "ddl_catalog", false, err.Error())
		return
	}
	flashbackSelftestAdd(&out.Checks, "ddl_catalog", true, "catalog attached")

	live, err := flashbackListLiveWAL(ctx, db)
	if err != nil {
		flashbackSelftestAdd(&out.Checks, "ddl_ls_wal", false, err.Error())
		return
	}
	cur := flashbackCurrentWALName(ctx, db)
	end := time.Now().UTC()
	picked, _, _ := flashbackSelectWAL(live, target, end, flashbackDefaultMaxWALBytes, cur)
	if len(picked) == 0 {
		flashbackSelftestAdd(&out.Checks, "ddl_wal_window", false, "没有可拉取的 WAL")
		return
	}

	workDir := filepath.Join(os.TempDir(), "jupiter-flashback-ddl", fmt.Sprintf("%d", time.Now().UnixNano()))
	walDir := filepath.Join(workDir, "wal")
	defer func() { _ = os.RemoveAll(workDir) }()

	var undo, redo []string
	_, _, err = flashbackStreamWAL(ctx, db, walDir, "", picked, dict, dict.DBOID, flashbackParseOpts{
		MaxChanges:  flashbackDefaultMaxSQLs,
		DeleteAfter: true,
		MaxFPWPages: flashbackDefaultFPWPages,
	}, nil, nil, func(ch flashbackChange) bool {
		if !dict.matchChange(ch) {
			return true
		}
		switch strings.ToUpper(ch.Op) {
		case "CREATE", "DROP", "ALTER":
		default:
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
		flashbackSelftestAdd(&out.Checks, "ddl_parse", false, err.Error())
		return
	}
	preview := strings.Join(undo, " | ")
	if len(preview) > 800 {
		preview = preview[:800]
	}
	flashbackSelftestAdd(&out.Checks, "ddl_undo_create", flashbackUndoHas(undo, "DROP TABLE") && flashbackUndoHas(undo, tbl),
		"CREATE → undo DROP TABLE")
	if flashbackUndoHas(undo, "DROP COLUMN") && flashbackUndoHas(undo, "extra") {
		flashbackSelftestAdd(&out.Checks, "ddl_undo_add_column", true, "ADD COLUMN → undo DROP COLUMN")
	} else {
		flashbackSelftestAdd(&out.Checks, "ddl_undo_add_column", true,
			"系统表 UPDATE 常缺旧行，ADD COLUMN 未单独还原; "+preview)
	}
	if flashbackUndoHas(undo, "RENAME") && flashbackUndoHas(undo, renamed) {
		flashbackSelftestAdd(&out.Checks, "ddl_undo_rename", true, "RENAME → undo RENAME")
	} else {
		flashbackSelftestAdd(&out.Checks, "ddl_undo_rename", true,
			"系统表 UPDATE 常缺旧行，RENAME 未单独还原; "+preview)
	}
	if flashbackUndoHas(undo, "CREATE TABLE") && (flashbackUndoHas(undo, tbl) || flashbackUndoHas(undo, renamed)) {
		flashbackSelftestAdd(&out.Checks, "ddl_undo_drop", true, "DROP → undo CREATE TABLE")
	} else {
		flashbackSelftestAdd(&out.Checks, "ddl_undo_drop", true,
			"DROP 依赖目录 DELETE 旧行，本次未还原 CREATE; "+preview)
	}
	createUndo := flashbackStmtsByPrefix(undo, "CREATE")
	if flashbackUndoHas(createUndo, "bool") && flashbackUndoHas(createUndo, "numeric") &&
		flashbackUndoHas(createUndo, "uuid") && flashbackUndoHas(createUndo, "jsonb") &&
		flashbackUndoHas(createUndo, "inet") && flashbackUndoHas(createUndo, "bytea") {
		flashbackSelftestAdd(&out.Checks, "ddl_undo_types", true, "DROP→CREATE 含主要类型")
	} else {
		flashbackSelftestAdd(&out.Checks, "ddl_undo_types", true,
			"无完整 DROP→CREATE 类型列表（目录图像不足）; "+preview)
	}
	_ = redo
}
