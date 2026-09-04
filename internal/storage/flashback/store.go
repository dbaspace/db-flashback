package flashback

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"unicode/utf8"

	"db-flashback/internal/storage/databases"
	v7 "db-flashback/pkg/utils/uuid/v7"
)

// Store 读写闪回任务 / 日志 / SQL。
type Store struct{}

func (Store) db() *sql.DB { return databases.GetRawDB() }

// NewID 生成任务主键。
func NewID() string { return v7.NewUUIDv7() }

// 闪回表由人工执行 change/sql/ 下全部脚本维护。
var flashbackRequiredTables = []string{
	"tbl_flashback_tasks",
	"tbl_flashback_logs",
	"tbl_flashback_sqls",
	"tbl_flashback_args",
	"tbl_flashback_instances",
	"tbl_flashback_artifacts",
}

func collectMissingFlashbackTables(found map[string]bool) []string {
	var missing []string
	for _, name := range flashbackRequiredTables {
		if !found[name] {
			missing = append(missing, name)
		}
	}
	return missing
}

func flashbackSchemaNotReadyErr(missing []string) error {
	return fmt.Errorf("闪回表未就绪（缺 %s），请先执行 change/sql/ 下全部脚本", strings.Join(missing, ", "))
}

var flashbackRequiredTaskCols = []string{
	"start_file", "start_pos", "stop_file", "stop_pos",
	"log_total", "log_done", "parse_total", "parse_done",
	"engine", "extra",
}

func flashbackAlterScriptsForCols(missing []string) []string {
	needBinlog, needProgress, needPDU := false, false, false
	for _, c := range missing {
		switch strings.ToLower(strings.TrimSpace(c)) {
		case "start_file", "start_pos", "stop_file", "stop_pos":
			needBinlog = true
		case "log_total", "log_done", "parse_total", "parse_done":
			needProgress = true
		case "engine", "extra":
			needPDU = true
		}
	}
	var scripts []string
	if needBinlog {
		scripts = append(scripts, "change/sql/tbl_flashback_alter_binlog_pos.sql")
	}
	if needProgress {
		scripts = append(scripts, "change/sql/tbl_flashback_alter_progress.sql")
	}
	if needPDU {
		scripts = append(scripts, "change/sql/tbl_flashback_pdu.sql")
	}
	if len(scripts) == 0 {
		scripts = append(scripts, "change/sql/tbl_flashback.sql")
	}
	return scripts
}

func flashbackSchemaColumnsNotReadyErr(missing []string) error {
	return fmt.Errorf("闪回表缺列 %s，请先由 DBA 执行 %s",
		strings.Join(missing, ", "), strings.Join(flashbackAlterScriptsForCols(missing), " 与 "))
}

const flashbackArgsDDL = `
CREATE TABLE IF NOT EXISTS tbl_flashback_args (
    key         VARCHAR(255) PRIMARY KEY,
    value       TEXT         NOT NULL DEFAULT '',
    description TEXT         NOT NULL DEFAULT '',
    updated_at  TIMESTAMPTZ  NOT NULL DEFAULT NOW()
)`

func (s Store) EnsureArgsTable(ctx context.Context) error {
	db := s.db()
	if db == nil {
		return fmt.Errorf("database not initialized")
	}
	_, err := db.ExecContext(ctx, flashbackArgsDDL)
	return err
}

// EnsureSchema 检查任务表是否就绪；多云参数表由进程按需创建。
func (s Store) EnsureSchema(ctx context.Context) error {
	db := s.db()
	if db == nil {
		return fmt.Errorf("database not initialized")
	}
	if err := s.EnsureArgsTable(ctx); err != nil {
		return fmt.Errorf("ensure tbl_flashback_args: %w", err)
	}
	if err := s.EnsureInstancesTable(ctx); err != nil {
		return fmt.Errorf("ensure tbl_flashback_instances: %w", err)
	}
	if err := s.EnsureUsersTable(ctx); err != nil {
		return fmt.Errorf("ensure tbl_flashback_users: %w", err)
	}
	if err := s.EnsurePDUSchema(ctx); err != nil {
		return fmt.Errorf("ensure pdu columns: %w", err)
	}
	missing, err := s.missingFlashbackTables(ctx)
	if err != nil {
		return err
	}
	if len(missing) > 0 {
		return flashbackSchemaNotReadyErr(missing)
	}
	cols, err := s.missingFlashbackTaskCols(ctx)
	if err != nil {
		return err
	}
	if len(cols) > 0 {
		return flashbackSchemaColumnsNotReadyErr(cols)
	}
	return nil
}

func (s Store) missingFlashbackTaskCols(ctx context.Context) ([]string, error) {
	found := make(map[string]bool, len(flashbackRequiredTaskCols))
	rows, err := s.db().QueryContext(ctx, `
SELECT column_name
FROM information_schema.columns
WHERE table_schema = 'public' AND table_name = 'tbl_flashback_tasks'`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		found[strings.ToLower(strings.TrimSpace(name))] = true
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return collectMissingFlashbackTaskCols(found), nil
}

func collectMissingFlashbackTaskCols(found map[string]bool) []string {
	var missing []string
	for _, name := range flashbackRequiredTaskCols {
		if !found[name] {
			missing = append(missing, name)
		}
	}
	return missing
}

func (s Store) missingFlashbackTables(ctx context.Context) ([]string, error) {
	found := make(map[string]bool, len(flashbackRequiredTables))
	for _, name := range flashbackRequiredTables {
		var reg sql.NullString
		if err := s.db().QueryRowContext(ctx, `SELECT to_regclass($1)::text`, "public."+name).Scan(&reg); err != nil {
			return nil, err
		}
		if reg.Valid && strings.TrimSpace(reg.String) != "" {
			found[name] = true
		}
	}
	return collectMissingFlashbackTables(found), nil
}

const taskCols = `id, instance_id, mdm_instance_id, host, port, database_name, tables,
  target_time, end_time, start_xid, stop_xid, start_file, start_pos, stop_file, stop_pos,
  sql_type, output_kind, COALESCE(NULLIF(engine, ''), 'native'), COALESCE(extra, '{}'), status,
  error_message, warning, work_dir, wal_bytes, wal_files, change_count,
  log_total, log_done, parse_total, parse_done, dml_ticket_id,
  created_by, created_at, updated_at, started_at, finished_at`

func (s Store) InsertTask(ctx context.Context, r *TaskRow) error {
	db := s.db()
	if db == nil {
		return fmt.Errorf("database not initialized")
	}
	engine := strings.TrimSpace(r.Engine)
	if engine == "" {
		engine = EngineNative
	}
	extra := strings.TrimSpace(r.Extra)
	if extra == "" {
		extra = "{}"
	}
	_, err := db.ExecContext(ctx, `
INSERT INTO tbl_flashback_tasks (
  id, instance_id, mdm_instance_id, host, port, database_name, tables,
  target_time, end_time, start_xid, stop_xid, start_file, start_pos, stop_file, stop_pos,
  sql_type, output_kind, engine, extra, status,
  error_message, warning, work_dir, wal_bytes, wal_files, change_count, dml_ticket_id,
  created_by, created_at, updated_at
) VALUES (
  $1,$2,$3,$4,$5,$6,$7,
  $8,$9,$10,$11,$12,$13,$14,$15,
  $16,$17,$18,$19,$20,
  $21,$22,$23,$24,$25,$26,$27,
  $28,NOW(),NOW()
)`,
		r.ID, r.InstanceID, r.MDMInstanceID, r.Host, r.Port, r.DatabaseName, r.Tables,
		r.TargetTime, r.EndTime, r.StartXID, r.StopXID, r.StartFile, r.StartPos, r.StopFile, r.StopPos,
		r.SQLType, r.OutputKind, engine, extra, r.Status,
		r.ErrorMessage, r.Warning, r.WorkDir, r.WALBytes, r.WALFiles, r.ChangeCount, r.DMLTicketID,
		r.CreatedBy)
	return err
}

func (s Store) GetTask(ctx context.Context, id string) (*TaskRow, error) {
	db := s.db()
	if db == nil {
		return nil, fmt.Errorf("database not initialized")
	}
	row := db.QueryRowContext(ctx, `SELECT `+taskCols+` FROM tbl_flashback_tasks WHERE id=$1`, id)
	r, err := scanTaskRow(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return r, err
}

func scanTaskRow(sc interface{ Scan(dest ...any) error }) (*TaskRow, error) {
	r := &TaskRow{}
	var end, updated, started, finished sql.NullTime
	err := sc.Scan(
		&r.ID, &r.InstanceID, &r.MDMInstanceID, &r.Host, &r.Port, &r.DatabaseName, &r.Tables,
		&r.TargetTime, &end, &r.StartXID, &r.StopXID, &r.StartFile, &r.StartPos, &r.StopFile, &r.StopPos,
		&r.SQLType, &r.OutputKind, &r.Engine, &r.Extra, &r.Status,
		&r.ErrorMessage, &r.Warning, &r.WorkDir, &r.WALBytes, &r.WALFiles, &r.ChangeCount,
		&r.LogTotal, &r.LogDone, &r.ParseTotal, &r.ParseDone, &r.DMLTicketID,
		&r.CreatedBy, &r.CreatedAt, &updated, &started, &finished)
	if err != nil {
		return nil, err
	}
	if end.Valid {
		t := end.Time
		r.EndTime = &t
	}
	if updated.Valid {
		t := updated.Time
		r.UpdatedAt = &t
	}
	if started.Valid {
		t := started.Time
		r.StartedAt = &t
	}
	if finished.Valid {
		t := finished.Time
		r.FinishedAt = &t
	}
	return r, nil
}

func (s Store) ListTasks(ctx context.Context, f TaskListFilter) ([]*TaskRow, int, error) {
	db := s.db()
	if db == nil {
		return nil, 0, fmt.Errorf("database not initialized")
	}
	var conds []string
	var args []any
	idx := 1
	if id := strings.TrimSpace(f.InstanceID); id != "" {
		conds = append(conds, fmt.Sprintf("instance_id=$%d", idx))
		args = append(args, id)
		idx++
	}
	if st := strings.TrimSpace(f.Status); st != "" {
		conds = append(conds, fmt.Sprintf("status=$%d", idx))
		args = append(args, st)
		idx++
	}
	if kw := strings.TrimSpace(f.Keyword); kw != "" {
		conds = append(conds, fmt.Sprintf(
			"(id ILIKE $%d OR database_name ILIKE $%d OR host ILIKE $%d OR tables ILIKE $%d)",
			idx, idx, idx, idx))
		args = append(args, "%"+kw+"%")
		idx++
	}
	where := ""
	if len(conds) > 0 {
		where = "WHERE " + strings.Join(conds, " AND ")
	}
	var total int
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM tbl_flashback_tasks "+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	limit := f.Limit
	if limit <= 0 {
		limit = 10
	}
	if limit > 200 {
		limit = 200
	}
	offset := f.Offset
	if offset < 0 {
		offset = 0
	}
	q := fmt.Sprintf(`SELECT %s FROM tbl_flashback_tasks %s ORDER BY created_at DESC LIMIT $%d OFFSET $%d`,
		taskCols, where, idx, idx+1)
	args = append(args, limit, offset)
	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var out []*TaskRow
	for rows.Next() {
		r, err := scanTaskRow(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, r)
	}
	return out, total, rows.Err()
}

func (s Store) UpdateStatus(ctx context.Context, id, status, errMsg, warning string) error {
	db := s.db()
	if db == nil {
		return fmt.Errorf("database not initialized")
	}
	markStart := status == StatusRunning
	markFinish := status == StatusSucceeded || status == StatusFailed || status == StatusCancelled
	_, err := db.ExecContext(ctx, `
UPDATE tbl_flashback_tasks
SET status=$2, error_message=$3, warning=$4, updated_at=NOW(),
    started_at = CASE WHEN $5 AND started_at IS NULL THEN NOW() ELSE started_at END,
    finished_at = CASE WHEN $6 THEN NOW() ELSE finished_at END
WHERE id=$1`, id, status, errMsg, warning, markStart, markFinish)
	return err
}

func (s Store) UpdateProgress(ctx context.Context, id, workDir string, walBytes int64, walFiles, changeCount int) error {
	db := s.db()
	if db == nil {
		return fmt.Errorf("database not initialized")
	}
	_, err := db.ExecContext(ctx, `
UPDATE tbl_flashback_tasks
SET work_dir=$2, wal_bytes=$3, wal_files=$4, change_count=$5, updated_at=NOW()
WHERE id=$1`, id, workDir, walBytes, walFiles, changeCount)
	return err
}

func (s Store) UpdateStageProgress(ctx context.Context, id string, logDone, logTotal, parseDone, parseTotal int) error {
	db := s.db()
	if db == nil {
		return fmt.Errorf("database not initialized")
	}
	_, err := db.ExecContext(ctx, `
UPDATE tbl_flashback_tasks
SET log_done=$2, log_total=$3, parse_done=$4, parse_total=$5, updated_at=NOW()
WHERE id=$1`, id, logDone, logTotal, parseDone, parseTotal)
	return err
}

func (s Store) SetDMLTicketID(ctx context.Context, id, ticketID string) error {
	db := s.db()
	if db == nil {
		return fmt.Errorf("database not initialized")
	}
	_, err := db.ExecContext(ctx, `
UPDATE tbl_flashback_tasks SET dml_ticket_id=$2, updated_at=NOW() WHERE id=$1`, id, ticketID)
	return err
}

func (s Store) UpdateInstanceIDs(ctx context.Context, id, instanceID, mdmInstanceID string) error {
	db := s.db()
	if db == nil {
		return fmt.Errorf("database not initialized")
	}
	_, err := db.ExecContext(ctx, `
UPDATE tbl_flashback_tasks SET instance_id=$2, mdm_instance_id=$3, updated_at=NOW() WHERE id=$1`,
		id, instanceID, mdmInstanceID)
	return err
}

func (s Store) InsertLog(ctx context.Context, taskID, level, message string) error {
	db := s.db()
	if db == nil {
		return fmt.Errorf("database not initialized")
	}
	_, err := db.ExecContext(ctx, `
INSERT INTO tbl_flashback_logs (task_id, level, message) VALUES ($1,$2,$3)`,
		taskID, level, message)
	return err
}

func (s Store) ListLogs(ctx context.Context, taskID string, limit int) ([]*LogRow, error) {
	db := s.db()
	if db == nil {
		return nil, fmt.Errorf("database not initialized")
	}
	if limit <= 0 {
		limit = 200
	}
	rows, err := db.QueryContext(ctx, `
SELECT id, task_id, level, message, created_at
FROM tbl_flashback_logs WHERE task_id=$1 ORDER BY id DESC LIMIT $2`, taskID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*LogRow
	for rows.Next() {
		r := &LogRow{}
		if err := rows.Scan(&r.ID, &r.TaskID, &r.Level, &r.Message, &r.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func flashbackSafeUTF8(s string) string {
	if utf8.ValidString(s) {
		return s
	}
	return strings.ToValidUTF8(s, `\uFFFD`)
}

func (s Store) InsertSQLs(ctx context.Context, rows []*SQLRow) error {
	if len(rows) == 0 {
		return nil
	}
	db := s.db()
	if db == nil {
		return fmt.Errorf("database not initialized")
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	stmt, err := tx.PrepareContext(ctx, `
INSERT INTO tbl_flashback_sqls (task_id, seq, kind, schema_name, table_name, op, xid, ts, statement, risk)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, r := range rows {
		if _, err := stmt.ExecContext(ctx, r.TaskID, r.Seq, r.Kind, r.SchemaName, r.TableName, r.Op, r.XID, r.TS, flashbackSafeUTF8(r.Statement), r.Risk); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// sqlListWhere 按任务、kind、原始 op 拼 WHERE。ops 为空表示不限操作。
func sqlListWhere(taskID, kind string, ops []string) (where string, args []any, nextIdx int) {
	args = []any{taskID}
	where = "WHERE task_id=$1"
	nextIdx = 2
	if k := strings.TrimSpace(kind); k != "" {
		where += fmt.Sprintf(" AND kind=$%d", nextIdx)
		args = append(args, k)
		nextIdx++
	}
	seen := map[string]struct{}{}
	var uniq []string
	for _, op := range ops {
		op = strings.ToUpper(strings.TrimSpace(op))
		if op == "" {
			continue
		}
		if _, ok := seen[op]; ok {
			continue
		}
		seen[op] = struct{}{}
		uniq = append(uniq, op)
	}
	if len(uniq) == 0 {
		return where, args, nextIdx
	}
	ph := make([]string, len(uniq))
	for i, op := range uniq {
		ph[i] = fmt.Sprintf("$%d", nextIdx)
		args = append(args, op)
		nextIdx++
	}
	where += " AND upper(op) IN (" + strings.Join(ph, ",") + ")"
	return where, args, nextIdx
}

func (s Store) ListSQLs(ctx context.Context, taskID, kind string, ops []string, offset, limit int) ([]*SQLRow, int, error) {
	db := s.db()
	if db == nil {
		return nil, 0, fmt.Errorf("database not initialized")
	}
	where, args, idx := sqlListWhere(taskID, kind, ops)
	var total int
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM tbl_flashback_sqls "+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	if limit <= 0 {
		limit = 50
	}
	if limit > 500 {
		limit = 500
	}
	if offset < 0 {
		offset = 0
	}
	q := fmt.Sprintf(`SELECT id, task_id, seq, kind, schema_name, table_name, op, xid, ts, statement, risk
FROM tbl_flashback_sqls %s ORDER BY seq ASC LIMIT $%d OFFSET $%d`, where, idx, idx+1)
	args = append(args, limit, offset)
	rs, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, 0, err
	}
	defer rs.Close()
	var out []*SQLRow
	for rs.Next() {
		r := &SQLRow{}
		var ts sql.NullTime
		if err := rs.Scan(&r.ID, &r.TaskID, &r.Seq, &r.Kind, &r.SchemaName, &r.TableName, &r.Op, &r.XID, &ts, &r.Statement, &r.Risk); err != nil {
			return nil, 0, err
		}
		if ts.Valid {
			t := ts.Time
			r.TS = &t
		}
		out = append(out, r)
	}
	return out, total, rs.Err()
}

func (s Store) ListAllSQLStatements(ctx context.Context, taskID, kind string, ops []string) ([]string, error) {
	db := s.db()
	if db == nil {
		return nil, fmt.Errorf("database not initialized")
	}
	where, args, _ := sqlListWhere(taskID, kind, ops)
	rows, err := db.QueryContext(ctx, `SELECT statement FROM tbl_flashback_sqls `+where+` ORDER BY seq ASC`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var stmt string
		if err := rows.Scan(&stmt); err != nil {
			return nil, err
		}
		out = append(out, stmt)
	}
	return out, rows.Err()
}

// FailStuckRunning 把进程内未跑完的 running 任务标失败（重启后不能续跑）。
func (s Store) FailStuckRunning(ctx context.Context, msg string) (int64, error) {
	db := s.db()
	if db == nil {
		return 0, fmt.Errorf("database not initialized")
	}
	res, err := db.ExecContext(ctx, `
UPDATE tbl_flashback_tasks
SET status=$1, error_message=$2, updated_at=NOW(), finished_at=NOW()
WHERE status=$3`, StatusFailed, msg, StatusRunning)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
