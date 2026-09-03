package service

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	mdmmodel "db-flashback/internal/mdmmodel"

	"db-flashback/internal/service/dto"
)

func TestFlashbackMySQLVarString(t *testing.T) {
	if flashbackMySQLVarString([]byte("ROW")) != "ROW" {
		t.Fatal("bytes")
	}
	if flashbackMySQLVarString(1) != "1" {
		t.Fatal("int")
	}
	if flashbackMySQLVarString(" FULL ") != "FULL" {
		t.Fatal("string")
	}
}

func TestFlashbackMySQLCredsFromEnv(t *testing.T) {
	t.Setenv("FLASHBACK_MYSQL_USER", "")
	t.Setenv("FLASHBACK_MYSQL_PASSWORD", "")
	if _, ok := flashbackMySQLCredsFromEnv("h", 3306, "fbtest"); ok {
		t.Fatal("empty env should be off")
	}
	t.Setenv("FLASHBACK_MYSQL_USER", "monitor")
	t.Setenv("FLASHBACK_MYSQL_PASSWORD", "x")
	t.Setenv("FLASHBACK_MYSQL_HOST", "mysql.test.example.com")
	t.Setenv("FLASHBACK_MYSQL_PORT", "3306")
	c, ok := flashbackMySQLCredsFromEnv("", 0, "fbtest")
	if !ok || c.User != "monitor" || c.Host != "mysql.test.example.com" || c.Port != 3306 || c.DBName != "fbtest" {
		t.Fatalf("env creds %+v ok=%v", c, ok)
	}
}

func TestFlashbackMySQLUnknownDatabase(t *testing.T) {
	if !flashbackMySQLUnknownDatabase(fmt.Errorf("Error 1049 (42000): Unknown database 'x'")) {
		t.Fatal("1049")
	}
	if flashbackMySQLUnknownDatabase(fmt.Errorf("access denied")) {
		t.Fatal("should not match")
	}
}

func TestFlashbackIsMySQL(t *testing.T) {
	if !flashbackIsMySQL(mdmmodel.MySQLDBTypeEnum) {
		t.Fatal("mysql enum")
	}
	if !flashbackIsMySQL("mysql") {
		t.Fatal("mysql string")
	}
	if flashbackIsMySQL(mdmmodel.PostgreSQLDBTypeEnum) {
		t.Fatal("postgres should not be mysql")
	}
	if !flashbackIsPostgres(mdmmodel.PostgreSQLDBTypeEnum) {
		t.Fatal("postgres enum")
	}
}

func TestFlashbackParseMySQLTable(t *testing.T) {
	db, tbl, err := flashbackParseMySQLTable("orders", "shop")
	if err != nil || db != "shop" || tbl != "orders" {
		t.Fatalf("got %s.%s err=%v", db, tbl, err)
	}
	db, tbl, err = flashbackParseMySQLTable("shop.orders", "other")
	if err != nil || db != "shop" || tbl != "orders" {
		t.Fatalf("got %s.%s err=%v", db, tbl, err)
	}
	db, tbl, err = flashbackParseMySQLTable("`shop`.`orders`", "")
	if err != nil || db != "shop" || tbl != "orders" {
		t.Fatalf("quoted got %s.%s err=%v", db, tbl, err)
	}
	if _, _, err = flashbackParseMySQLTable("a.b.c", "x"); err == nil {
		t.Fatal("expected error")
	}
	if _, _, err = flashbackParseMySQLTable("only", ""); err == nil {
		t.Fatal("expected missing db")
	}
}

func TestFlashbackMySQLFormatGate(t *testing.T) {
	st, _ := flashbackMySQLFormatGate("ROW", "FULL")
	if st != flashbackCheckPassed {
		t.Fatalf("row+full should pass, got %s", st)
	}
	st, msg := flashbackMySQLFormatGate("MIXED", "FULL")
	if st != flashbackCheckFailed || !strings.Contains(msg, "MIXED") && !strings.Contains(msg, "STATEMENT") {
		t.Fatalf("mixed should fail: %s %s", st, msg)
	}
	st, msg = flashbackMySQLFormatGate("ROW", "MINIMAL")
	if st != flashbackCheckFailed || !strings.Contains(msg, "MINIMAL") {
		t.Fatalf("minimal should fail: %s %s", st, msg)
	}
	st, msg = flashbackMySQLFormatGate("STATEMENT", "FULL")
	if st != flashbackCheckFailed {
		t.Fatalf("statement should fail: %s %s", st, msg)
	}
}

func TestFlashbackMySQLHasReplPriv(t *testing.T) {
	slave, client := flashbackMySQLHasReplPriv([]string{
		"GRANT SELECT ON `app`.* TO `u`@`%`",
	})
	if slave || client {
		t.Fatal("select only should not have repl")
	}
	slave, client = flashbackMySQLHasReplPriv([]string{
		"GRANT REPLICATION SLAVE, REPLICATION CLIENT ON *.* TO `u`@`%`",
	})
	if !slave || !client {
		t.Fatal("classic repl grants")
	}
	slave, client = flashbackMySQLHasReplPriv([]string{
		"GRANT ALL PRIVILEGES ON *.* TO `root`@`%`",
	})
	if !slave || !client {
		t.Fatal("all privileges")
	}
	slave, client = flashbackMySQLHasReplPriv([]string{
		"GRANT BINLOG MONITOR, REPLICATION_SLAVE_ADMIN ON *.* TO `u`@`%`",
	})
	if !slave || !client {
		t.Fatal("8.0+ names")
	}
	slave, client = flashbackMySQLHasReplPriv([]string{
		"GRANT ALL PRIVILEGES ON `app`.* TO `u`@`%`",
	})
	if slave || client {
		t.Fatal("db-level all should not count as dump priv")
	}
}

func TestFlashbackMySQLDumpServerID(t *testing.T) {
	a := flashbackMySQLDumpServerID("task-a")
	b := flashbackMySQLDumpServerID("task-b")
	if a < 100000000 || a >= 200000000 {
		t.Fatalf("id out of range %d", a)
	}
	if a == b {
		t.Fatal("different tasks should hash differently")
	}
	if flashbackMySQLDumpServerID("same") != flashbackMySQLDumpServerID("same") {
		t.Fatal("stable hash")
	}
	avoid := flashbackMySQLDumpServerID("hit")
	got := flashbackMySQLDumpServerID("hit", avoid)
	if got == avoid {
		t.Fatal("should avoid collision")
	}
}

func TestFlashbackMySQLPickStartIndex(t *testing.T) {
	now := time.Date(2026, 8, 29, 16, 0, 0, 0, time.UTC)
	expire := 7 * 24 * time.Hour
	if idx := flashbackMySQLPickStartIndex(1, now.Add(-time.Hour), now, expire); idx != 0 {
		t.Fatalf("single file idx=%d", idx)
	}
	if idx := flashbackMySQLPickStartIndex(10, now.Add(-expire), now, expire); idx != 0 {
		t.Fatalf("oldest target idx=%d", idx)
	}
	idx := flashbackMySQLPickStartIndex(20, now.Add(-time.Hour), now, expire)
	if idx < 0 || idx >= 20 {
		t.Fatalf("idx out of range %d", idx)
	}
	if flashbackMySQLPickStartIndex(20, now, now, 0) != 0 {
		t.Fatal("no expire starts at 0")
	}
}

func TestFlashbackMySQLPickStartIndexByTimes(t *testing.T) {
	t0 := time.Date(2026, 8, 31, 10, 0, 0, 0, time.UTC)
	t1 := t0.Add(time.Hour)
	t2 := t0.Add(2 * time.Hour)
	t3 := t0.Add(3 * time.Hour)
	files := []time.Time{t0, t1, t2, t3}
	if got := flashbackMySQLPickStartIndexByTimes(files, t0.Add(90*time.Minute)); got != 1 {
		t.Fatalf("window inside file1, got %d", got)
	}
	if got := flashbackMySQLPickStartIndexByTimes(files, t2); got != 2 {
		t.Fatalf("exact file start, got %d", got)
	}
	if got := flashbackMySQLPickStartIndexByTimes(files, t0.Add(-time.Minute)); got != 0 {
		t.Fatalf("before first file, got %d", got)
	}
	if got := flashbackMySQLPickStartIndexByTimes(files, t3.Add(time.Hour)); got != 3 {
		t.Fatalf("after last first-ts, got %d", got)
	}
	partial := []time.Time{{}, {}, t2, t3}
	if got := flashbackMySQLPickStartIndexByTimes(partial, t2.Add(-time.Minute)); got != 2 {
		t.Fatalf("unknown prefix should start at first known after target, got %d", got)
	}
	if flashbackMySQLPickStartIndexByTimes(nil, t0) != 0 {
		t.Fatal("empty")
	}
}

func TestFlashbackMySQLPosReached(t *testing.T) {
	if !flashbackMySQLPosReached("mysql-bin.000003", 4, "mysql-bin.000002", 100) {
		t.Fatal("later file should be reached")
	}
	if !flashbackMySQLPosReached("mysql-bin.000002", 200, "mysql-bin.000002", 100) {
		t.Fatal("same file later pos")
	}
	if flashbackMySQLPosReached("mysql-bin.000001", 50, "mysql-bin.000002", 100) {
		t.Fatal("earlier file")
	}
}

func TestFlashbackMySQLLiteral(t *testing.T) {
	if flashbackMySQLLiteral(nil, "int") != "NULL" {
		t.Fatal("nil")
	}
	if flashbackMySQLLiteral(int64(42), "int") != `\RAW:42` {
		t.Fatalf("int %s", flashbackMySQLLiteral(int64(42), "int"))
	}
	if flashbackMySQLLiteral("hello", "varchar") != "hello" {
		t.Fatal("string")
	}
	got := flashbackMySQLLiteral([]byte{0xde, 0xad}, "blob")
	if !strings.Contains(strings.ToLower(got), "dead") {
		t.Fatalf("blob %s", got)
	}
	js := flashbackMySQLLiteral([]byte(`{"k":2}`), "json")
	if js != `{"k":2}` {
		t.Fatalf("json %s", js)
	}
	bin := flashbackMySQLLiteral([]byte{0xbe, 0xef}, "varchar")
	if !strings.Contains(strings.ToLower(bin), "beef") {
		t.Fatalf("invalid utf8 should hex, got %s", bin)
	}
}

func TestFlashbackMySQLIsDDLQuery(t *testing.T) {
	if !flashbackMySQLIsDDLQuery("CREATE TABLE t (id int)") {
		t.Fatal("create")
	}
	if !flashbackMySQLIsDDLQuery("truncate table t") {
		t.Fatal("truncate")
	}
	if flashbackMySQLIsDDLQuery("BEGIN") || flashbackMySQLIsDDLQuery("COMMIT") {
		t.Fatal("begin/commit")
	}
	if flashbackMySQLIsDDLQuery("INSERT INTO t VALUES (1)") {
		t.Fatal("insert")
	}
	if flashbackMySQLDDLOp("ALTER TABLE t ADD c int") != "ALTER" {
		t.Fatal("alter op")
	}
	if flashbackMySQLDDLOp("RENAME TABLE a TO b") != "ALTER" {
		t.Fatal("rename as alter")
	}
	if flashbackMySQLTxnBoundary("START TRANSACTION") != "begin" {
		t.Fatal("start transaction")
	}
	if flashbackMySQLTxnBoundary("ROLLBACK") != "rollback" {
		t.Fatal("rollback")
	}
}

func TestFlashbackUndoMySQLQuoting(t *testing.T) {
	ch := flashbackChange{
		Schema: "shop", Table: "t", Op: "INSERT", MySQL: true,
		New:    map[string]string{"id": `\RAW:1`, "name": "a"},
		PKCols: []string{"id"},
	}
	stmt, risk := flashbackUndoSQL(ch)
	if risk != "" {
		t.Fatalf("risk %s", risk)
	}
	if stmt != "DELETE FROM `shop`.`t` WHERE `id` = 1;" {
		t.Fatalf("stmt=%s", stmt)
	}
	ch = flashbackChange{
		Schema: "shop", Table: "t", Op: "DELETE", MySQL: true,
		Old: map[string]string{"id": `\RAW:2`, "name": "b"},
	}
	stmt, _ = flashbackUndoSQL(ch)
	if !strings.Contains(stmt, "INSERT INTO `shop`.`t`") || !strings.Contains(stmt, "2") {
		t.Fatalf("stmt=%s", stmt)
	}
	ch = flashbackChange{
		Schema: "shop", Table: "t", Op: "UPDATE", MySQL: true,
		Old:    map[string]string{"id": `\RAW:1`, "name": "old"},
		New:    map[string]string{"id": `\RAW:1`, "name": "new"},
		PKCols: []string{"id"},
	}
	stmt, _ = flashbackUndoSQL(ch)
	if !strings.Contains(stmt, "UPDATE `shop`.`t` SET") || !strings.Contains(stmt, "`name` = 'old'") {
		t.Fatalf("stmt=%s", stmt)
	}
	if !strings.Contains(stmt, "WHERE `id` = 1") {
		t.Fatalf("stmt=%s", stmt)
	}
}

func TestFlashbackUndoMySQLNoPKRisk(t *testing.T) {
	ch := flashbackChange{
		Schema: "shop", Table: "t", Op: "INSERT", MySQL: true, NoPK: true,
		New: map[string]string{"id": `\RAW:1`, "name": "a"},
	}
	_, risk := flashbackUndoSQL(ch)
	if !strings.Contains(risk, "无主键") {
		t.Fatalf("risk=%s", risk)
	}
}

func TestFlashbackMySQLHostingStatus(t *testing.T) {
	st, msg := flashbackMySQLHostingStatus()
	if st != flashbackCheckPassed {
		t.Fatalf("mysql hosting should pass, got %s", st)
	}
	if !strings.Contains(msg, "在线模式") || strings.Contains(msg, "rds.aliyuncs.com") || strings.Contains(msg, "仅支持自建") {
		t.Fatalf("mysql hosting should be generic online: %s", msg)
	}
}

func TestFlashbackMySQLParseModeOnline(t *testing.T) {
	if flashbackParseModeOnline != "online" || flashbackParseModeFile != "file" {
		t.Fatalf("parse mode constants: %s %s", flashbackParseModeOnline, flashbackParseModeFile)
	}
	out := &dto.FlashbackPrecheckResult{}
	out.ParseMode = flashbackParseModeOnline
	if out.ParseMode != "online" {
		t.Fatalf("parse_mode=%s", out.ParseMode)
	}
}

func TestFlashbackMySQLRejectOfflineWorkDir(t *testing.T) {
	if err := flashbackMySQLRejectOfflineWorkDir(""); err != nil {
		t.Fatalf("empty workdir: %v", err)
	}
	err := flashbackMySQLRejectOfflineWorkDir("/tmp/hub-binlog")
	if err == nil || !strings.Contains(err.Error(), "在线模式") {
		t.Fatalf("any local binlog dir should fail, got %v", err)
	}
}

func TestFlashbackMySQLDumpProbeMessage(t *testing.T) {
	if flashbackMySQLDumpProbeMessage(nil) != "BINLOG DUMP 可用" {
		t.Fatal("nil err")
	}
	msg := flashbackMySQLDumpProbeMessage(fmt.Errorf("Access denied; you need (at least one of) the REPLICATION SLAVE privilege"))
	if !strings.Contains(msg, "BINLOG DUMP 失败") || !strings.Contains(msg, "控制台") {
		t.Fatalf("msg=%s", msg)
	}
}

func TestFlashbackMySQLProbeDumpEmptyFile(t *testing.T) {
	err := flashbackMySQLProbeDump(context.Background(), flashbackMySQLCreds{Host: "127.0.0.1", Port: 3306}, "", 4)
	if err == nil || !strings.Contains(err.Error(), "empty binlog file") {
		t.Fatalf("expected empty file error, got %v", err)
	}
	_, err = flashbackMySQLFirstEventTime(context.Background(), flashbackMySQLCreds{Host: "127.0.0.1", Port: 3306}, "")
	if err == nil || !strings.Contains(err.Error(), "empty binlog file") {
		t.Fatalf("first event empty file: %v", err)
	}
}

func TestFlashbackMySQLEventWindow(t *testing.T) {
	const target, end uint32 = 1000, 1010
	skip, stop := flashbackMySQLEventWindow(999, target, end)
	if !skip || stop {
		t.Fatalf("before window: skip=%v stop=%v", skip, stop)
	}
	skip, stop = flashbackMySQLEventWindow(1000, target, end)
	if skip || stop {
		t.Fatalf("at start: skip=%v stop=%v", skip, stop)
	}
	skip, stop = flashbackMySQLEventWindow(1010, target, end)
	if skip || stop {
		t.Fatalf("at end inclusive: skip=%v stop=%v", skip, stop)
	}
	skip, stop = flashbackMySQLEventWindow(1011, target, end)
	if !skip || !stop {
		t.Fatalf("after window: skip=%v stop=%v", skip, stop)
	}
	skip, stop = flashbackMySQLEventWindow(0, target, end)
	if skip || stop {
		t.Fatalf("zero ts should continue scan: skip=%v stop=%v", skip, stop)
	}
}

func TestFlashbackMySQLSelftestAssertTimeWindow(t *testing.T) {
	sql := flashbackMySQLSelftestOutsideInsert("`fb`.`t`", 99, flashbackMySQLSelftestBeforeMark)
	if !strings.Contains(sql, flashbackMySQLSelftestBeforeMark) || !strings.Contains(sql, "99") {
		t.Fatalf("outside insert: %s", sql)
	}
	out := &dto.FlashbackSelftestResult{}
	flashbackMySQLSelftestAssertTimeWindow(out,
		[]string{"UPDATE t SET c_text='hello' WHERE id=1", "DELETE FROM t WHERE id=2"},
		[]string{"UPDATE t SET c_text='world' WHERE id=1", "INSERT INTO t (id) VALUES (1)"},
	)
	for _, c := range out.Checks {
		if !c.OK {
			t.Fatalf("clean window should pass: %+v", c)
		}
	}
	out = &dto.FlashbackSelftestResult{}
	flashbackMySQLSelftestAssertTimeWindow(out,
		[]string{"DELETE FROM t WHERE c_text='before_window'"},
		[]string{"INSERT INTO t (c_text) VALUES ('after_window')"},
	)
	failed := map[string]bool{}
	for _, c := range out.Checks {
		if !c.OK {
			failed[c.Name] = true
		}
	}
	if !failed["time_window_include"] || !failed["time_window_exclude_before"] || !failed["time_window_exclude_after"] {
		t.Fatalf("leaked window markers should fail, checks=%+v", out.Checks)
	}
}

func TestFlashbackMySQLResolveDumpRange(t *testing.T) {
	logs := []flashbackMySQLBinlogFile{
		{Name: "mysql-bin.000001"},
		{Name: "mysql-bin.000002"},
		{Name: "mysql-bin.000003"},
	}
	file, pos, endFile, endPos, err := flashbackMySQLResolveDumpRange(
		"", 0, "", 0, logs, "mysql-bin.000002", "mysql-bin.000003", 88)
	if err != nil || file != "mysql-bin.000002" || pos != 4 || endFile != "mysql-bin.000003" || endPos != 88 {
		t.Fatalf("auto range file=%s pos=%d end=%s:%d err=%v", file, pos, endFile, endPos, err)
	}
	file, pos, endFile, endPos, err = flashbackMySQLResolveDumpRange(
		"mysql-bin.000001", 120, "mysql-bin.000002", 40, logs, "ignored", "mysql-bin.000003", 88)
	if err != nil || file != "mysql-bin.000001" || pos != 120 || endFile != "mysql-bin.000002" || endPos != 40 {
		t.Fatalf("explicit range file=%s pos=%d end=%s:%d err=%v", file, pos, endFile, endPos, err)
	}
	file, pos, endFile, endPos, err = flashbackMySQLResolveDumpRange(
		"mysql-bin.000002", 0, "mysql-bin.000002", 0, logs, "", "mysql-bin.000003", 1)
	if err != nil || pos != 4 || endPos != ^uint32(0) {
		t.Fatalf("default pos file=%s pos=%d endPos=%d err=%v", file, pos, endPos, err)
	}
	if _, _, _, _, err = flashbackMySQLResolveDumpRange("no-such", 4, "", 0, logs, "", "", 0); err == nil {
		t.Fatal("missing start_file should fail")
	}
}

func TestFlashbackMySQLXIDInRange(t *testing.T) {
	if !flashbackMySQLXIDInRange(10, 0, 0) {
		t.Fatal("no filter")
	}
	if flashbackMySQLXIDInRange(5, 10, 0) || flashbackMySQLXIDInRange(0, 10, 0) {
		t.Fatal("below start or unknown xid")
	}
	if !flashbackMySQLXIDInRange(10, 10, 20) || !flashbackMySQLXIDInRange(20, 10, 20) {
		t.Fatal("inclusive bounds")
	}
	if flashbackMySQLXIDInRange(21, 10, 20) {
		t.Fatal("past stop")
	}
	if !flashbackMySQLXIDPastStop(21, 20) || flashbackMySQLXIDPastStop(20, 20) {
		t.Fatal("past stop helper")
	}
}

func TestFlashbackMySQLDictionarySnapshotRoundtrip(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/dict.json"
	dict := &flashbackMySQLDict{
		DBName: "shop",
		Wanted: map[string]*flashbackMySQLRel{
			"shop.t": {
				Schema: "shop", Name: "t", PKCols: []string{"id"},
				Columns: []flashbackMySQLCol{
					{Name: "id", DataType: "int", ColumnType: "int(11)", Charset: ""},
					{Name: "name", DataType: "varchar", ColumnType: "varchar(32)", Charset: "utf8mb4"},
				},
			},
		},
	}
	if err := flashbackSaveMySQLDictionaryFile(path, dict); err != nil {
		t.Fatal(err)
	}
	got, err := flashbackLoadMySQLDictionaryFile(path)
	if err != nil {
		t.Fatal(err)
	}
	rel := got.match("shop", "t")
	if rel == nil || len(rel.Columns) != 2 || rel.Columns[1].Charset != "utf8mb4" || rel.PKCols[0] != "id" {
		t.Fatalf("%+v", rel)
	}
	if _, err := flashbackLoadMySQLDictionaryFile("/no/such/dict.json"); err == nil {
		t.Fatal("missing file")
	}
	pgPath := dir + "/pg.json"
	if err := flashbackSaveDictionaryFile(pgPath, &flashbackDictionary{DBName: "db", Wanted: map[string]*flashbackRelation{}}); err != nil {
		t.Fatal(err)
	}
	if _, err := flashbackLoadMySQLDictionaryFile(pgPath); err == nil {
		t.Fatal("pg snap must not load as mysql")
	}
}

func TestFlashbackMySQLDDLAuditSQL(t *testing.T) {
	ch := flashbackChange{
		Schema: "shop", Op: "CREATE", MySQL: true,
		DDLRedo: "CREATE TABLE t (id int);",
		DDLRisk: "MySQL 仅审计原文，不做反向闪回",
	}
	undo, _ := flashbackUndoSQL(ch)
	if undo != "" {
		t.Fatalf("ddl must not generate undo: %s", undo)
	}
	redo, risk := flashbackRedoSQL(ch)
	if redo != "CREATE TABLE t (id int);" || !strings.Contains(risk, "审计") {
		t.Fatalf("redo=%s risk=%s", redo, risk)
	}
	if !flashbackSQLIsDDLStatement(redo) || flashbackSQLIsDDLStatement("DELETE FROM t WHERE id=1") {
		t.Fatal("ddl statement detect")
	}
}

func TestFlashbackChangeXID(t *testing.T) {
	if flashbackChangeXID(flashbackChange{XID: 7}) != 7 {
		t.Fatal("pg xid")
	}
	if flashbackChangeXID(flashbackChange{XID: 7, XID64: 99}) != 99 {
		t.Fatal("mysql xid64 wins")
	}
}

func TestFlashbackValidateBinlogName(t *testing.T) {
	if err := flashbackValidateBinlogName("mysql-bin.000001", "start_file"); err != nil {
		t.Fatal(err)
	}
	if err := flashbackValidateBinlogName("../etc/passwd", "start_file"); err == nil {
		t.Fatal("path traversal")
	}
	if err := flashbackValidateBinlogName("a/b", "stop_file"); err == nil {
		t.Fatal("slash")
	}
}

func TestFlashbackMySQLFindBinlog(t *testing.T) {
	logs := []flashbackMySQLBinlogFile{{Name: "mysql-bin.000001"}, {Name: "mysql-bin.000002"}}
	if flashbackMySQLFindBinlog(logs, "mysql-bin.000002") != 1 {
		t.Fatal("find")
	}
	if flashbackMySQLFindBinlog(logs, "x") != -1 {
		t.Fatal("missing")
	}
	if flashbackMySQLNormalizeStartPos(0) != 4 || flashbackMySQLNormalizeStartPos(100) != 100 {
		t.Fatal("normalize pos")
	}
}

func TestFlashbackMySQLChangesSQLAndOriginal(t *testing.T) {
	changes := []flashbackChange{
		{Schema: "fb", Table: "ta", Op: "INSERT", MySQL: true, New: map[string]string{"id": `\RAW:1`}, PKCols: []string{"id"}},
		{Schema: "fb", Table: "ta", Op: "INSERT", MySQL: true, New: map[string]string{"id": `\RAW:2`}, PKCols: []string{"id"}},
		{Schema: "fb", Table: "ta", Op: "UPDATE", MySQL: true, Old: map[string]string{"id": `\RAW:1`, "c": "hello"}, New: map[string]string{"id": `\RAW:1`, "c": "world"}, PKCols: []string{"id"}},
		{Schema: "fb", Table: "ta", Op: "DELETE", MySQL: true, Old: map[string]string{"id": `\RAW:2`}, PKCols: []string{"id"}},
		{Schema: "fb", Table: "tb", Op: "INSERT", MySQL: true, New: map[string]string{"id": `\RAW:1`}, PKCols: []string{"id"}},
		{Schema: "fb", Table: "tb", Op: "INSERT", MySQL: true, New: map[string]string{"id": `\RAW:2`}, PKCols: []string{"id"}},
		{Schema: "fb", Table: "tb", Op: "UPDATE", MySQL: true, Old: map[string]string{"id": `\RAW:1`}, New: map[string]string{"id": `\RAW:1`}, PKCols: []string{"id"}},
		{Schema: "fb", Table: "tb", Op: "DELETE", MySQL: true, Old: map[string]string{"id": `\RAW:2`}, PKCols: []string{"id"}},
		{Schema: "fb", Op: "CREATE", MySQL: true, DDLRedo: "CREATE TABLE t (id int);", DDLRisk: "audit"},
	}
	_, ins := flashbackMySQLChangesSQL(changes, []string{"fb.ta"}, "INSERT")
	if len(ins) != 2 {
		t.Fatalf("single insert redo=%d", len(ins))
	}
	_, allIns := flashbackMySQLChangesSQL(changes, nil, "INSERT")
	if len(allIns) != 4 {
		t.Fatalf("all insert redo=%d", len(allIns))
	}
	u, r := flashbackMySQLChangesSQL(changes, nil, "CREATE")
	if len(u) != 0 || len(r) != 1 {
		t.Fatalf("create undo=%d redo=%d", len(u), len(r))
	}
	out := &dto.FlashbackSelftestResult{}
	flashbackMySQLAssertOriginalByType(out, "fb", "ta", "tb", changes)
	for _, c := range out.Checks {
		if !c.OK {
			t.Fatalf("%s: %s", c.Name, c.Detail)
		}
	}
}

func TestFlashbackMySQLSelftestAddPrecheckWindow(t *testing.T) {
	out := &dto.FlashbackSelftestResult{}
	flashbackMySQLSelftestAddPrecheckWindow(out, &dto.FlashbackPrecheckResult{
		ParseMode: flashbackParseModeOnline,
		Items: []dto.FlashbackCheckItem{{
			Code: "time_window", Name: "时间窗", Status: flashbackCheckPassed, Message: "仅解析窗口",
		}},
	})
	if !flashbackSelftestOK(out.Checks) {
		t.Fatalf("online+time_window should pass: %+v", out.Checks)
	}
	out = &dto.FlashbackSelftestResult{}
	flashbackMySQLSelftestAddPrecheckWindow(out, &dto.FlashbackPrecheckResult{ParseMode: flashbackParseModeFile})
	if flashbackSelftestOK(out.Checks) {
		t.Fatal("file mode / missing time_window should fail")
	}
}
