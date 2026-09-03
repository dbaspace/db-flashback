package service

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	mdmmodel "db-flashback/internal/mdmmodel"

	"db-flashback/internal/service/dto"
	"db-flashback/internal/storage/flashback"
)

func TestFlashbackLooksLikeCloudHost(t *testing.T) {
	cases := []struct {
		host string
		want bool
	}{
		{"pg.internal.example.com", false},
		{"10.0.0.8", false},
		{"rm-bp1xxxx.pg.rds.aliyuncs.com", true},
		{"cdb-xxxx.postgres.tencentcdb.com", true},
		{"mydb.rds.amazonaws.com:5432", true},
	}
	for _, tc := range cases {
		if got := flashbackLooksLikeCloudHost(tc.host); got != tc.want {
			t.Fatalf("host %s: got %v want %v", tc.host, got, tc.want)
		}
	}
}

func TestFlashbackCloudVersionReason(t *testing.T) {
	if got := flashbackCloudVersionReason("5.7.18-txsql-log"); got == "" || !strings.Contains(got, "txsql") {
		t.Fatalf("txsql should be cloud edition for PG gate: %s", got)
	}
	if flashbackCloudVersionReason("5.7.42-log") != "" {
		t.Fatal("community mysql should not be cloud by version")
	}
}

func TestFlashbackCloudReasonTags(t *testing.T) {
	res := &mdmmodel.ResourceDbsInfo{
		Address: "10.1.1.1:5432",
		Tags:    map[string]interface{}{"vendor": "aliyun"},
	}
	if got := flashbackCloudReason(res); got == "" {
		t.Fatal("expected cloud reason from vendor tag")
	}
	self := &mdmmodel.ResourceDbsInfo{Address: "pg.prod.local:5432"}
	if got := flashbackCloudReason(self); got != "" {
		t.Fatalf("self-hosted got reason %s", got)
	}
}

func TestFlashbackCloudRoleReason(t *testing.T) {
	if flashbackCloudRoleReason([]string{"app_user"}) != "" {
		t.Fatal("app_user should not be cloud")
	}
	if flashbackCloudRoleReason([]string{"rds_superuser"}) == "" {
		t.Fatal("rds_superuser should be detected")
	}
}

func TestFlashbackWalLevelGate(t *testing.T) {
	st, _ := flashbackWalLevelGate("logical", false)
	if st != flashbackCheckPassed {
		t.Fatalf("logical should pass, got %s", st)
	}
	st, msg := flashbackWalLevelGate("replica", true)
	if st != flashbackCheckWarning || !strings.Contains(msg, "replica") {
		t.Fatalf("replica+fpw should warn: %s %s", st, msg)
	}
	st, msg = flashbackWalLevelGate("replica", false)
	if st != flashbackCheckFailed || !strings.Contains(msg, "full_page_writes") {
		t.Fatalf("replica without fpw should fail: %s %s", st, msg)
	}
	st, msg = flashbackWalLevelGate("minimal", true)
	if st != flashbackCheckFailed || !strings.Contains(msg, "minimal") {
		t.Fatalf("minimal should fail: %s %s", st, msg)
	}
}

func TestFlashbackLiveHasCurrent(t *testing.T) {
	live := []flashbackWALFile{{Name: "000000010000000000000001", Source: "live"}}
	if !flashbackLiveHasCurrent(live, "000000010000000000000001") {
		t.Fatal("expected current live segment")
	}
	if flashbackLiveHasCurrent(live, "000000010000000000000002") {
		t.Fatal("other segment should not match")
	}
	if flashbackLiveHasCurrent(nil, "") {
		t.Fatal("empty current should be false")
	}
}

func TestFlashbackParseTableName(t *testing.T) {
	s, n, err := flashbackParseTableName("orders")
	if err != nil || s != "public" || n != "orders" {
		t.Fatalf("got %s.%s err=%v", s, n, err)
	}
	s, n, err = flashbackParseTableName("shop.orders")
	if err != nil || s != "shop" || n != "orders" {
		t.Fatalf("got %s.%s err=%v", s, n, err)
	}
	if _, _, err = flashbackParseTableName("a.b.c"); err == nil {
		t.Fatal("expected error")
	}
}

func TestFlashbackUndoInsertNoPKUsesCTID(t *testing.T) {
	ch := flashbackChange{
		Schema: "public", Table: "t", Op: "INSERT",
		New: map[string]string{"name": "a"}, NoPK: true,
		NewBlock: 3, NewOff: 7,
	}
	stmt, risk := flashbackUndoSQL(ch)
	if !strings.Contains(stmt, `ctid = '(3,7)'`) {
		t.Fatalf("stmt=%s", stmt)
	}
	if !strings.Contains(risk, "ctid") {
		t.Fatalf("risk=%s", risk)
	}
}

func TestFlashbackUndoInsert(t *testing.T) {
	ch := flashbackChange{
		Schema: "public", Table: "t", Op: "INSERT",
		New:    map[string]string{"id": "1", "name": "a"},
		PKCols: []string{"id"},
	}
	stmt, risk := flashbackUndoSQL(ch)
	if risk != "" {
		t.Fatalf("risk %s", risk)
	}
	if stmt != `DELETE FROM "public"."t" WHERE "id" = '1';` {
		t.Fatalf("stmt=%s", stmt)
	}
}

func TestFlashbackUndoDelete(t *testing.T) {
	ch := flashbackChange{
		Schema: "public", Table: "t", Op: "DELETE",
		Old: map[string]string{"id": "2", "name": "b"},
	}
	stmt, _ := flashbackUndoSQL(ch)
	if !strings.Contains(stmt, `INSERT INTO "public"."t"`) || !strings.Contains(stmt, `'2'`) {
		t.Fatalf("stmt=%s", stmt)
	}
}

func TestFlashbackUndoUpdate(t *testing.T) {
	ch := flashbackChange{
		Schema: "public", Table: "t", Op: "UPDATE",
		Old:    map[string]string{"id": "1", "name": "old"},
		New:    map[string]string{"id": "1", "name": "new"},
		PKCols: []string{"id"},
	}
	stmt, _ := flashbackUndoSQL(ch)
	if !strings.Contains(stmt, `UPDATE "public"."t" SET`) || !strings.Contains(stmt, `"name" = 'old'`) {
		t.Fatalf("stmt=%s", stmt)
	}
	if !strings.Contains(stmt, `WHERE "id" = '1'`) {
		t.Fatalf("stmt=%s", stmt)
	}
}

func TestFlashbackWalMinerLimits(t *testing.T) {
	var items []dto.FlashbackCheckItem
	dict := &flashbackDictionary{Wanted: map[string]*flashbackRelation{
		"public.t": {
			Schema: "public", Name: "t", OID: 100, RelNode: 200, ReplIdent: "d",
			Columns: []flashbackColumn{
				{Name: "id", TypeName: "int4"},
				{Name: "........pg.dropped.2........", Dropped: true},
			},
		},
	}}
	flashbackAddWalMinerLimits(&items, dict, "")
	want := map[string]string{
		"scope_dml": "warning", "dict_snapshot": "warning", "timeline": "passed",
		"bulk_copy": "warning", "undo_match": "warning", "rewrite": "warning",
		"dropped_cols": "warning", "no_unique": "warning",
		"ddl_catalog_image": "warning",
	}
	got := map[string]string{}
	for _, it := range items {
		got[it.Code] = it.Status
	}
	for code, st := range want {
		if got[code] != st {
			t.Fatalf("check %s: got %s want %s items=%v", code, got[code], st, got)
		}
	}
	if !strings.Contains(func() string {
		for _, it := range items {
			if it.Code == "rewrite" {
				return it.Message
			}
		}
		return ""
	}(), "relfilenode") {
		t.Fatal("rewrite should mention relfilenode mismatch")
	}
}

func TestFlashbackUndoSkipsDroppedCol(t *testing.T) {
	ch := flashbackChange{
		Schema: "public", Table: "t", Op: "DELETE",
		Old: map[string]string{
			"id": "1", "name": "a",
			"........pg.dropped.3........": `\RAW:encode('\xad97'::bytea, 'hex')`,
		},
	}
	stmt, _ := flashbackUndoSQL(ch)
	if strings.Contains(stmt, "pg.dropped") || strings.Contains(stmt, `'encode`) {
		t.Fatalf("dropped col leaked into undo: %s", stmt)
	}
	if !strings.Contains(stmt, `"id"`) || !strings.Contains(stmt, `"name"`) {
		t.Fatalf("expected live columns: %s", stmt)
	}
}

func TestFlashbackValidateReq(t *testing.T) {
	req := dto.FlashbackTaskReq{
		InstanceID: "11111111-1111-1111-1111-111111111111",
		Database:   "app",
		Tables:     []string{"public.t"},
		TargetTime: time.Now().Add(-time.Hour).Format(time.RFC3339),
	}
	if err := flashbackValidateReq(&req); err != nil {
		t.Fatal(err)
	}
	req.Tables = nil
	if err := flashbackValidateReq(&req); err != nil {
		t.Fatalf("empty tables means whole database: %v", err)
	}
	req.Tables = []string{"", "  "}
	if err := flashbackValidateReq(&req); err != nil {
		t.Fatalf("blank tables means whole database: %v", err)
	}
	req.Tables = []string{"a.b.c"}
	if err := flashbackValidateReq(&req); err == nil {
		t.Fatal("expected invalid table name")
	}
	req.Tables = []string{"public.t"}
	req.StartPos = 120
	if err := flashbackValidateReq(&req); err == nil {
		t.Fatal("start_pos without start_file")
	}
	req.StartPos = 0
	req.StartFile = "mysql-bin.000003"
	req.StopFile = "mysql-bin.000001"
	if err := flashbackValidateReq(&req); err == nil {
		t.Fatal("start_file after stop_file")
	}
	req.StopFile = "mysql-bin.000003"
	req.StartPos = 200
	req.StopPos = 80
	if err := flashbackValidateReq(&req); err == nil {
		t.Fatal("stop_pos before start_pos")
	}
	req.StopPos = 400
	if err := flashbackValidateReq(&req); err != nil {
		t.Fatalf("valid file:pos: %v", err)
	}
}

func TestFlashbackNormalizeTableNames(t *testing.T) {
	if got := flashbackNormalizeTableNames([]string{" public.t ", "", "shop.o"}); len(got) != 2 || got[0] != "public.t" || got[1] != "shop.o" {
		t.Fatalf("got %#v", got)
	}
	if !flashbackTablesIsAll(nil) || !flashbackTablesIsAll([]string{"", " "}) {
		t.Fatal("empty/blank should be all tables")
	}
	if flashbackTablesIsAll([]string{"public.t"}) {
		t.Fatal("specified table is not all")
	}
	if flashbackTablesJSON(nil) != "[]" || flashbackTablesJSON([]string{"", "a"}) != `["a"]` {
		t.Fatalf("json=%s %s", flashbackTablesJSON(nil), flashbackTablesJSON([]string{"", "a"}))
	}
}

func TestFlashbackSelftestAssertOriginalSQL(t *testing.T) {
	out := &dto.FlashbackSelftestResult{}
	flashbackSelftestAssertOriginalSQL(out, "orig_single_insert", "INSERT", "tbl_a", "tbl_b", "single",
		[]string{`INSERT INTO "public"."tbl_a" VALUES (1)`}, 1)
	flashbackSelftestAssertOriginalSQL(out, "orig_multi_delete", "DELETE", "tbl_a", "tbl_b", "multi",
		[]string{`DELETE FROM "public"."tbl_a" WHERE id=1`, `DELETE FROM "public"."tbl_b" WHERE id=1`}, 2)
	for _, c := range out.Checks {
		if !c.OK {
			t.Fatalf("%s: %s", c.Name, c.Detail)
		}
	}
}

func TestFlashbackSelftestOutputKind(t *testing.T) {
	if flashbackSelftestOutputKind("original") != "original" {
		t.Fatal("original")
	}
	if flashbackSelftestOutputKind("") != "flashback" {
		t.Fatal("default")
	}
}

func TestFlashbackSelftestAssertTableScope(t *testing.T) {
	out := &dto.FlashbackSelftestResult{}
	flashbackSelftestAssertTableScope(out, "tbl_a", "tbl_b",
		[]string{`UPDATE "public"."tbl_a" SET x=1`},
		[]string{`UPDATE "public"."tbl_a" SET x=1`, `UPDATE "public"."tbl_b" SET x=1`},
		[]string{`DELETE FROM "public"."tbl_a"`, `DELETE FROM "public"."tbl_b"`},
	)
	for _, c := range out.Checks {
		if !c.OK {
			t.Fatalf("scope %s failed: %s", c.Name, c.Detail)
		}
	}
}

func TestFlashbackAddTableScopeCheck(t *testing.T) {
	var items []dto.FlashbackCheckItem
	flashbackAddTableScopeCheck(&items, nil, 3)
	if len(items) == 0 || items[0].Name != "整库表" || items[0].Status != flashbackCheckPassed || !strings.Contains(items[0].Message, "整库 3 张表") {
		t.Fatalf("all-db check: %+v", items)
	}
	items = nil
	flashbackAddTableScopeCheck(&items, []string{"public.t"}, 1)
	if len(items) == 0 || items[0].Name != "单表" || !strings.Contains(items[0].Message, "单表 public.t") {
		t.Fatalf("specified check: %+v", items)
	}
	items = nil
	flashbackAddTableScopeCheck(&items, nil, flashbackAllTablesWarnMin)
	if len(items) < 2 || items[1].Code != "table_count" || items[1].Status != flashbackCheckWarning {
		t.Fatalf("many tables should warn: %+v", items)
	}
}

func TestFlashbackIsWALSegName(t *testing.T) {
	if !flashbackIsWALSegName("000000010000000000000001") {
		t.Fatal("expected valid")
	}
	if flashbackIsWALSegName("000000010000000000000001.partial") {
		t.Fatal("partial should be invalid")
	}
}

func TestFlashbackWantOp(t *testing.T) {
	f := flashbackNormalizeSQLTypes("insert,delete")
	if !flashbackWantOp(f, "INSERT") || flashbackWantOp(f, "UPDATE") {
		t.Fatal("filter mismatch")
	}
	if !flashbackWantOp(nil, "UPDATE") {
		t.Fatal("empty filter should allow all")
	}
	ddl := flashbackNormalizeSQLTypes("ddl")
	if !flashbackWantOp(ddl, "CREATE") || !flashbackWantOp(ddl, "DROP") || !flashbackWantOp(ddl, "ALTER") {
		t.Fatal("ddl filter should match CREATE/DROP/ALTER")
	}
	if flashbackWantOp(ddl, "INSERT") {
		t.Fatal("ddl filter should not match INSERT")
	}
}

func TestFlashbackSQLPreviewFilter(t *testing.T) {
	kind, ops := flashbackSQLPreviewFilter("flashback", "delete", "", "")
	if kind != "undo" || len(ops) != 1 || ops[0] != "DELETE" {
		t.Fatalf("delete+反向: kind=%s ops=%v", kind, ops)
	}
	kind, ops = flashbackSQLPreviewFilter("original", "update", "", "")
	if kind != "redo" || len(ops) != 1 || ops[0] != "UPDATE" {
		t.Fatalf("update+正向: kind=%s ops=%v", kind, ops)
	}
	kind, ops = flashbackSQLPreviewFilter("flashback", "", "", "")
	if kind != "undo" || ops != nil {
		t.Fatalf("空 sql_type+反向: kind=%s ops=%v", kind, ops)
	}
	kind, ops = flashbackSQLPreviewFilter("flashback", "insert,update,delete", "redo", "insert")
	if kind != "redo" || len(ops) != 1 || ops[0] != "INSERT" {
		t.Fatalf("query 覆盖: kind=%s ops=%v", kind, ops)
	}
	kind, ops = flashbackSQLPreviewFilter("flashback", "ddl", "", "")
	if kind != "undo" {
		t.Fatalf("ddl kind=%s", kind)
	}
	joined := strings.Join(ops, ",")
	for _, want := range []string{"CREATE", "DROP", "ALTER", "TRUNCATE"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("ddl ops=%v missing %s", ops, want)
		}
	}

	// 与截图同形：undo/redo 各一对 UPDATE/DELETE，按任务过滤。
	rows := []flashback.SQLRow{
		{Kind: "undo", Op: "UPDATE", Statement: "UPDATE t SET x=old"},
		{Kind: "redo", Op: "UPDATE", Statement: "UPDATE t SET x=new"},
		{Kind: "undo", Op: "DELETE", Statement: "INSERT INTO t ..."},
		{Kind: "redo", Op: "DELETE", Statement: "DELETE FROM t ..."},
	}
	pick := func(outputKind, sqlType string) []string {
		k, o := flashbackSQLPreviewFilter(outputKind, sqlType, "", "")
		allow := map[string]struct{}{}
		for _, op := range o {
			allow[op] = struct{}{}
		}
		var out []string
		for _, r := range rows {
			if r.Kind != k {
				continue
			}
			if len(allow) > 0 {
				if _, ok := allow[r.Op]; !ok {
					continue
				}
			}
			out = append(out, r.Statement)
		}
		return out
	}
	got := pick("flashback", "delete")
	if len(got) != 1 || !strings.HasPrefix(got[0], "INSERT") {
		t.Fatalf("delete+反向 want 1 INSERT, got %v", got)
	}
	got = pick("original", "update")
	if len(got) != 1 || !strings.Contains(got[0], "x=new") {
		t.Fatalf("update+正向 want redo UPDATE, got %v", got)
	}
	got = pick("flashback", "")
	if len(got) != 2 {
		t.Fatalf("空 sql_type+反向 want 2 undo, got %v", got)
	}
	for _, s := range got {
		if strings.HasPrefix(s, "DELETE FROM") || strings.Contains(s, "x=new") {
			t.Fatalf("反向结果不应含 redo: %v", got)
		}
	}
}

func TestFlashbackSelftestHelpers(t *testing.T) {
	checks := []dto.FlashbackSelftestCheck{{Name: "a", OK: true}, {Name: "b", OK: true}}
	if !flashbackSelftestOK(checks) {
		t.Fatal("expected all ok")
	}
	checks[1].OK = false
	if flashbackSelftestOK(checks) {
		t.Fatal("expected fail")
	}
	stmts := []string{
		`UPDATE "public"."t" SET "c_text" = 'hello' WHERE "id" = '1';`,
		`INSERT INTO "public"."t" ("c_arr") VALUES ('{9}');`,
	}
	if !flashbackUndoHas(flashbackStmtsByPrefix(stmts, "UPDATE"), "'hello'") {
		t.Fatal("update hello")
	}
	if !flashbackUndoHas(flashbackStmtsByPrefix(stmts, "INSERT"), "{9}") {
		t.Fatal("insert array")
	}
}

func TestFlashbackSelftestTableHas80Types(t *testing.T) {
	sql := flashbackSelftestTableSQL("public.t")
	n := 0
	for _, line := range strings.Split(sql, "\n") {
		s := strings.TrimSpace(line)
		if s == "" || strings.HasPrefix(s, "CREATE") || s == ")" || s == ");" {
			continue
		}
		n++
	}
	if n < flashbackSelftestMinCols {
		t.Fatalf("selftest table has %d cols, want >= %d\n%s", n, flashbackSelftestMinCols, sql)
	}
	for _, typ := range []string{"boolean", "smallint", "bigint", "numeric", "decimal", "real", "double precision",
		"money", "smallserial", "serial", "bigserial", "text", "varchar", "char(", `"char"`, "name", "bytea",
		"date", "time", "timetz", "timestamp", "timestamptz", "interval year to month",
		"uuid", "json", "jsonb", "xml", "inet", "cidr", "macaddr", "macaddr8", "bit(", "varbit",
		"oid", "xid", "cid", "pg_lsn", "txid_snapshot", "regclass", "regtype", "regrole",
		"integer[]", "text[]", "jsonb[]", "point", "circle", "line", "tsvector", "tsquery",
		"int4range", "daterange", "jsonpath", "c_enum", "c_composite"} {
		if !strings.Contains(sql, typ) {
			t.Fatalf("missing type %s in selftest table", typ)
		}
	}
	extra := flashbackSelftestPGExtraCols(16)
	if !strings.Contains(extra, "int4multirange") || !strings.Contains(extra, "pg_snapshot") {
		t.Fatalf("pg14 extra missing: %s", extra)
	}
	if flashbackSelftestPGExtraCols(13) != "" {
		t.Fatal("pg13 should not add multirange")
	}
}

func TestFlashbackCleanupTaskDir(t *testing.T) {
	base := t.TempDir()
	taskID := "01a046a2-144e-787f-ad10-be7abc9795b8"
	dir := filepath.Join(base, taskID, "wal")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "000000010000000000000001"), []byte("wal"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := flashbackCleanupTaskDir(base, taskID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(base, taskID)); !os.IsNotExist(err) {
		t.Fatalf("expected task dir removed, err=%v", err)
	}
	if err := flashbackCleanupTaskDir(base, "../etc"); err == nil {
		t.Fatal("expected refuse traversal")
	}
	if err := flashbackCleanupTaskDir("/", "etc"); err == nil {
		t.Fatal("expected refuse root")
	}
}

func TestFlashbackSplitXLogBodyInsert(t *testing.T) {
	relNode := uint32(12345)
	dbNode := uint32(16384)
	spc := uint32(1663)
	tuple := flashbackTestXLogTuple(1, "a")
	var body []byte
	body = append(body, 0)          // block id
	body = append(body, bkpHasData) // flags
	dl := make([]byte, 2)
	binary.LittleEndian.PutUint16(dl, uint16(len(tuple)))
	body = append(body, dl...)
	put32 := func(v uint32) {
		b := make([]byte, 4)
		binary.LittleEndian.PutUint32(b, v)
		body = append(body, b...)
	}
	put32(spc)
	put32(dbNode)
	put32(relNode)
	put32(0) // blkno
	body = append(body, xlrBlockIDDataShort, byte(flashbackSizeOfHeapInsert))
	body = append(body, tuple...)
	body = append(body, 2, 0, 0) // offnum=2, flags=0
	blocks, main, _, ok := flashbackSplitXLogBody(body)
	if !ok || len(blocks) != 1 {
		t.Fatalf("split ok=%v blocks=%d", ok, len(blocks))
	}
	if blocks[0].relNode != relNode || blocks[0].dbNode != dbNode {
		t.Fatalf("rnode %+v", blocks[0])
	}
	if len(blocks[0].data) != len(tuple) {
		t.Fatalf("data len %d want %d", len(blocks[0].data), len(tuple))
	}
	if len(main) != 3 {
		t.Fatalf("main len %d", len(main))
	}
}

func TestFlashbackDecodeXLogTuple(t *testing.T) {
	rel := &flashbackRelation{
		Schema: "public", Name: "tbl_test", RelNode: 12345,
		PKCols: []string{"id"},
		Columns: []flashbackColumn{
			{Name: "id", AttNum: 1, TypeName: "int4", Typlen: 4, Typalign: "i", IsPK: true},
			{Name: "cname", AttNum: 2, TypeName: "text", Typlen: -1, Typalign: "i"},
		},
	}
	vals := flashbackDecodeXLogTuple(rel, flashbackTestXLogTuple(1, "a"))
	if vals["id"] != "1" || vals["cname"] != "a" {
		t.Fatalf("vals=%v", vals)
	}
}

func TestFlashbackDecodeHeapRecordInsert(t *testing.T) {
	rel := &flashbackRelation{
		Schema: "public", Name: "tbl_test", RelNode: 12345, DBOID: 16384,
		PKCols: []string{"id"},
		Columns: []flashbackColumn{
			{Name: "id", AttNum: 1, TypeName: "int4", Typlen: 4, Typalign: "i", IsPK: true},
			{Name: "cname", AttNum: 2, TypeName: "text", Typlen: -1, Typalign: "i"},
		},
	}
	dict := &flashbackDictionary{
		DBOID:     16384,
		ByRelNode: map[uint32]*flashbackRelation{12345: rel},
		Wanted:    map[string]*flashbackRelation{"public.tbl_test": rel},
	}
	tuple := flashbackTestXLogTuple(7, "g")
	var body []byte
	body = append(body, 0, bkpHasData)
	dl := make([]byte, 2)
	binary.LittleEndian.PutUint16(dl, uint16(len(tuple)))
	body = append(body, dl...)
	put32 := func(v uint32) {
		b := make([]byte, 4)
		binary.LittleEndian.PutUint32(b, v)
		body = append(body, b...)
	}
	put32(1663)
	put32(16384)
	put32(12345)
	put32(0)
	body = append(body, xlrBlockIDDataShort, byte(flashbackSizeOfHeapInsert))
	body = append(body, tuple...)
	body = append(body, 8, 0, 0)

	rec := make([]byte, flashbackSizeOfXLogRecord+len(body))
	binary.LittleEndian.PutUint32(rec[0:4], uint32(len(rec)))
	binary.LittleEndian.PutUint32(rec[4:8], 42)
	rec[16] = xlogHeapInsert
	rec[17] = rmHeap
	copy(rec[flashbackSizeOfXLogRecord:], body)

	ch := flashbackDecodeHeapRecord(rec, dict, 16384, nil, nil)
	if len(ch) != 1 || ch[0].Op != "INSERT" || ch[0].New["id"] != "7" || ch[0].New["cname"] != "g" {
		t.Fatalf("got %+v", ch)
	}
}

func TestFlashbackLooksLikeWALMagic(t *testing.T) {
	for _, magic := range []uint16{0xD110, 0xD113, 0xD116, 0xD118} {
		if !flashbackLooksLikeWALMagic(magic) {
			t.Fatalf("PG15–18 magic 0x%04X must be accepted", magic)
		}
	}
	if flashbackLooksLikeWALMagic(0) || flashbackLooksLikeWALMagic(0x1234) {
		t.Fatal("non-WAL magic must be rejected")
	}
}

func TestFlashbackVersionGate(t *testing.T) {
	if flashbackParseServerMajor("15.6") != 15 || flashbackParseServerMajor("16.4 (Ubuntu)") != 16 {
		t.Fatal("parse major")
	}
	if flashbackMagicMajor(0xD110) != 15 || flashbackMagicMajor(0xD113) != 16 ||
		flashbackMagicMajor(0xD116) != 17 || flashbackMagicMajor(0xD118) != 18 ||
		flashbackMagicMajor(0xD121) != 19 {
		t.Fatal("magic major")
	}
	st, _ := flashbackVersionGate("15.6")
	if st != flashbackCheckPassed {
		t.Fatalf("15 should pass, got %s", st)
	}
	st, msg := flashbackVersionGate("16.4")
	if st != flashbackCheckPassed || !strings.Contains(msg, "矩阵") {
		t.Fatalf("16 should pass as matrix-verified: %s %s", st, msg)
	}
	st, _ = flashbackVersionGate("18.0")
	if st != flashbackCheckPassed {
		t.Fatalf("18 should pass, got %s", st)
	}
	st, _ = flashbackVersionGate("14.12")
	if st != flashbackCheckPassed {
		t.Fatalf("14 should pass after matrix, got %s", st)
	}
	st, _ = flashbackVersionGate("11.22")
	if st != flashbackCheckFailed {
		t.Fatalf("11 should fail, got %s", st)
	}
	st, msg = flashbackVersionGate("19beta3")
	if st != flashbackCheckPassed || !strings.Contains(msg, "0xD121") {
		t.Fatalf("19 beta should pass after matrix: %s %s", st, msg)
	}
	if xlogHeap2MultiInsert != 0x50 {
		t.Fatalf("official XLOG_HEAP2_MULTI_INSERT is 0x50, got 0x%02X", xlogHeap2MultiInsert)
	}
	if !strings.Contains(flashbackVersionImpactSummary(17), "0x50") {
		t.Fatal("17 impact should mention MULTI_INSERT 0x50")
	}
}

func TestFlashbackSkipUpdatePrefixSuffix(t *testing.T) {
	raw := []byte{0x01, 0x00, 0x02, 0x00, 0xaa, 0xbb}
	got := flashbackSkipUpdatePrefixSuffix(raw, xlhUpdatePrefixFromOld|xlhUpdateSuffixFromOld)
	if len(got) != 2 || got[0] != 0xaa {
		t.Fatalf("got %x", got)
	}
	if flashbackSkipUpdatePrefixSuffix(raw, 0)[0] != 0x01 {
		t.Fatal("no flags should keep bytes")
	}
}

func TestFlashbackDecodeMultiInsertOpcodeAndInitPage(t *testing.T) {
	rel := &flashbackRelation{
		Schema: "public", Name: "t", PKCols: []string{"id"},
		Columns: []flashbackColumn{{Name: "id", TypeName: "int4", Typlen: 4, Typalign: "i", IsPK: true}},
	}
	// xl_multi_insert_tuple: datalen=4 + int4，t_hoff=24（官方重建 HeapTuple 的数据起点）。
	body := make([]byte, 8+4)
	binary.LittleEndian.PutUint16(body[0:2], 4)
	body[6] = 24
	binary.LittleEndian.PutUint32(body[8:12], 9)
	main := []byte{xlhInsertContainsNewTuple, 1, 0} // flags, ntuples=1, INIT_PAGE 省略 offsets
	main = append(main, body...)
	ch := flashbackDecodeMultiInsert(rel, 11, nil, main, xlogHeapInitPage, func(c flashbackChange) []flashbackChange {
		return []flashbackChange{c}
	})
	if len(ch) != 1 || ch[0].New["id"] != "9" {
		t.Fatalf("INIT_PAGE multi-insert: %+v", ch)
	}
}

func TestFlashbackParseWALFilePG15Magic(t *testing.T) {
	rel := &flashbackRelation{
		Schema: "public", Name: "tbl_test", RelNode: 12345, DBOID: 16384,
		PKCols: []string{"id"},
		Columns: []flashbackColumn{
			{Name: "id", AttNum: 1, TypeName: "int4", Typlen: 4, Typalign: "i", IsPK: true},
			{Name: "cname", AttNum: 2, TypeName: "text", Typlen: -1, Typalign: "i"},
		},
	}
	dict := &flashbackDictionary{
		DBOID:     16384,
		ByRelNode: map[uint32]*flashbackRelation{12345: rel},
		Wanted:    map[string]*flashbackRelation{"public.tbl_test": rel},
	}
	tuple := flashbackTestXLogTuple(2, "b")
	var recBody []byte
	recBody = append(recBody, 0, bkpHasData)
	dl := make([]byte, 2)
	binary.LittleEndian.PutUint16(dl, uint16(len(tuple)))
	recBody = append(recBody, dl...)
	put32 := func(dst *[]byte, v uint32) {
		b := make([]byte, 4)
		binary.LittleEndian.PutUint32(b, v)
		*dst = append(*dst, b...)
	}
	put32(&recBody, 1663)
	put32(&recBody, 16384)
	put32(&recBody, 12345)
	put32(&recBody, 0)
	recBody = append(recBody, xlrBlockIDDataShort, byte(flashbackSizeOfHeapInsert))
	recBody = append(recBody, tuple...)
	recBody = append(recBody, 2, 0, 0)

	tot := flashbackSizeOfXLogRecord + len(recBody)
	aligned := (tot + 7) &^ 7
	page := make([]byte, flashbackXLogPageSize)
	binary.LittleEndian.PutUint16(page[0:2], 0xD110) // PG 15
	binary.LittleEndian.PutUint16(page[2:4], xlpLongHeader)
	rec := page[40 : 40+aligned]
	binary.LittleEndian.PutUint32(rec[0:4], uint32(tot))
	binary.LittleEndian.PutUint32(rec[4:8], 99)
	rec[16] = xlogHeapInsert
	rec[17] = rmHeap
	copy(rec[flashbackSizeOfXLogRecord:], recBody)

	dir := t.TempDir()
	path := filepath.Join(dir, "000000010000000000000001")
	if err := os.WriteFile(path, page, 0o600); err != nil {
		t.Fatal(err)
	}
	ch, st, err := flashbackParseWALDir(dir, dict, 16384)
	if err != nil {
		t.Fatal(err)
	}
	if st.Records == 0 || st.MagicSkip > 0 {
		t.Fatalf("stats %+v", st)
	}
	if len(ch) != 1 || ch[0].Op != "INSERT" || ch[0].New["id"] != "2" {
		t.Fatalf("got %+v stats=%s", ch, st.String())
	}
	for _, magic := range []uint16{0xD113, 0xD116, 0xD118} {
		binary.LittleEndian.PutUint16(page[0:2], magic)
		dir2 := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir2, "000000010000000000000001"), page, 0o600); err != nil {
			t.Fatal(err)
		}
		ch2, st2, err := flashbackParseWALDir(dir2, dict, 16384)
		if err != nil || st2.MagicSkip > 0 || len(ch2) != 1 || ch2[0].New["id"] != "2" {
			t.Fatalf("magic 0x%04X: err=%v stats=%+v ch=%+v", magic, err, st2, ch2)
		}
	}
}

func flashbackTestXLogTuple(id int32, name string) []byte {
	// xl_heap_header + pad to t_hoff=24 + int4 + 1-byte varlena text
	raw := make([]byte, 5)
	binary.LittleEndian.PutUint16(raw[0:2], 2) // natts
	binary.LittleEndian.PutUint16(raw[2:4], 0) // infomask
	raw[4] = 24                                // t_hoff
	raw = append(raw, 0)                       // pad orig[23]
	b := make([]byte, 4)
	binary.LittleEndian.PutUint32(b, uint32(id))
	raw = append(raw, b...)
	vh := byte((len(name)+1)<<1 | 1)
	raw = append(raw, vh)
	raw = append(raw, name...)
	return raw
}

func TestFlashbackParseWALFileSpanningFPW(t *testing.T) {
	rel := &flashbackRelation{
		Schema: "public", Name: "tbl_test", RelNode: 24740, DBOID: 24622,
		PKCols: []string{"id"},
		Columns: []flashbackColumn{
			{Name: "id", AttNum: 1, TypeName: "int4", Typlen: 4, Typalign: "i", IsPK: true},
			{Name: "cname", AttNum: 2, TypeName: "text", Typlen: -1, Typalign: "i"},
		},
	}
	dict := &flashbackDictionary{
		DBOID:     24622,
		ByRelNode: map[uint32]*flashbackRelation{24740: rel},
		Wanted:    map[string]*flashbackRelation{"public.tbl_test": rel},
	}
	tuple := flashbackTestXLogTuple(2, "b")
	img := make([]byte, 8100)
	var recBody []byte
	recBody = append(recBody, 0, bkpHasImage|bkpHasData)
	dl := make([]byte, 2)
	binary.LittleEndian.PutUint16(dl, uint16(len(tuple)))
	recBody = append(recBody, dl...)
	il := make([]byte, 2)
	binary.LittleEndian.PutUint16(il, uint16(len(img)))
	recBody = append(recBody, il...)
	recBody = append(recBody, 0, 0, 0) // hole_offset, bimg_info
	put32 := func(v uint32) {
		b := make([]byte, 4)
		binary.LittleEndian.PutUint32(b, v)
		recBody = append(recBody, b...)
	}
	put32(1663)
	put32(24622)
	put32(24740)
	put32(0)
	recBody = append(recBody, xlrBlockIDDataShort, byte(flashbackSizeOfHeapInsert))
	recBody = append(recBody, img...)
	recBody = append(recBody, tuple...)
	recBody = append(recBody, 2, 0, 0)

	tot := flashbackSizeOfXLogRecord + len(recBody)
	recFull := make([]byte, tot)
	binary.LittleEndian.PutUint32(recFull[0:4], uint32(tot))
	binary.LittleEndian.PutUint32(recFull[4:8], 7)
	recFull[16] = xlogHeapInsert
	recFull[17] = rmHeap
	copy(recFull[flashbackSizeOfXLogRecord:], recBody)

	first := flashbackXLogPageSize - 40
	if tot <= first {
		t.Fatalf("need spanning record tot=%d first=%d", tot, first)
	}
	page0 := make([]byte, flashbackXLogPageSize)
	binary.LittleEndian.PutUint16(page0[0:2], 0xD110)
	binary.LittleEndian.PutUint16(page0[2:4], xlpLongHeader)
	copy(page0[40:], recFull[:first])

	page1 := make([]byte, flashbackXLogPageSize)
	binary.LittleEndian.PutUint16(page1[0:2], 0xD110)
	binary.LittleEndian.PutUint16(page1[2:4], xlpFirstContRecord)
	binary.LittleEndian.PutUint32(page1[16:20], uint32(tot-first))
	copy(page1[24:], recFull[first:])

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "000000010000000000000001"), append(page0, page1...), 0o600); err != nil {
		t.Fatal(err)
	}
	ch, st, err := flashbackParseWALDir(dir, dict, 24622)
	if err != nil {
		t.Fatal(err)
	}
	if st.Matched == 0 || len(ch) == 0 {
		t.Fatalf("expected spanning FPW insert, stats=%s ch=%v", st.String(), ch)
	}
	if ch[0].Op != "INSERT" || ch[0].New["id"] != "2" {
		t.Fatalf("got %+v", ch[0])
	}
}

func flashbackTestInsertRecord(xid, db, rel uint32, id int32, name string, img []byte) []byte {
	tuple := flashbackTestXLogTuple(id, name)
	var recBody []byte
	flags := byte(bkpHasData)
	if len(img) > 0 {
		flags |= bkpHasImage
	}
	recBody = append(recBody, 0, flags)
	dl := make([]byte, 2)
	binary.LittleEndian.PutUint16(dl, uint16(len(tuple)))
	recBody = append(recBody, dl...)
	if len(img) > 0 {
		il := make([]byte, 2)
		binary.LittleEndian.PutUint16(il, uint16(len(img)))
		recBody = append(recBody, il...)
		recBody = append(recBody, 0, 0, 0)
	}
	put32 := func(v uint32) {
		b := make([]byte, 4)
		binary.LittleEndian.PutUint32(b, v)
		recBody = append(recBody, b...)
	}
	put32(1663)
	put32(db)
	put32(rel)
	put32(0)
	recBody = append(recBody, xlrBlockIDDataShort, byte(flashbackSizeOfHeapInsert))
	if len(img) > 0 {
		recBody = append(recBody, img...)
	}
	recBody = append(recBody, tuple...)
	recBody = append(recBody, 2, 0, 0)
	tot := flashbackSizeOfXLogRecord + len(recBody)
	rec := make([]byte, tot)
	binary.LittleEndian.PutUint32(rec[0:4], uint32(tot))
	binary.LittleEndian.PutUint32(rec[4:8], xid)
	rec[16] = xlogHeapInsert
	rec[17] = rmHeap
	copy(rec[flashbackSizeOfXLogRecord:], recBody)
	return rec
}

func TestFlashbackParseWALFileRecordAfterSpanning(t *testing.T) {
	rel := &flashbackRelation{
		Schema: "public", Name: "tbl_test", RelNode: 24740, DBOID: 24622,
		PKCols: []string{"id"},
		Columns: []flashbackColumn{
			{Name: "id", AttNum: 1, TypeName: "int4", Typlen: 4, Typalign: "i", IsPK: true},
			{Name: "cname", AttNum: 2, TypeName: "text", Typlen: -1, Typalign: "i"},
		},
	}
	dict := &flashbackDictionary{
		DBOID:     24622,
		ByRelNode: map[uint32]*flashbackRelation{24740: rel},
		Wanted:    map[string]*flashbackRelation{"public.tbl_test": rel},
	}
	rec1 := flashbackTestInsertRecord(7, 24622, 24740, 2, "b", make([]byte, 8100))
	rec2 := flashbackTestInsertRecord(8, 24622, 24740, 9, "z", nil)
	first := flashbackXLogPageSize - 40
	if len(rec1) <= first {
		t.Fatalf("need spanning record tot=%d first=%d", len(rec1), first)
	}
	page0 := make([]byte, flashbackXLogPageSize)
	binary.LittleEndian.PutUint16(page0[0:2], 0xD110)
	binary.LittleEndian.PutUint16(page0[2:4], xlpLongHeader)
	copy(page0[40:], rec1[:first])

	rem := len(rec1) - first
	skip := flashbackMaxAlign(rem)
	if 24+skip+len(rec2) > flashbackXLogPageSize {
		t.Fatalf("rec2 does not fit rem=%d skip=%d rec2=%d", rem, skip, len(rec2))
	}
	page1 := make([]byte, flashbackXLogPageSize)
	binary.LittleEndian.PutUint16(page1[0:2], 0xD110)
	binary.LittleEndian.PutUint16(page1[2:4], xlpFirstContRecord)
	binary.LittleEndian.PutUint32(page1[16:20], uint32(rem))
	copy(page1[24:], rec1[first:])
	copy(page1[24+skip:], rec2)

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "000000010000000000000001"), append(page0, page1...), 0o600); err != nil {
		t.Fatal(err)
	}
	ch, st, err := flashbackParseWALDir(dir, dict, 24622)
	if err != nil {
		t.Fatal(err)
	}
	if len(ch) != 2 {
		t.Fatalf("want 2 inserts after spanning FPW, got %d stats=%s", len(ch), st.String())
	}
	if ch[0].New["id"] != "2" || ch[1].New["id"] != "9" {
		t.Fatalf("got %+v %+v", ch[0].New, ch[1].New)
	}
}

func TestFlashbackSelectWALIncludesStaleLive(t *testing.T) {
	old := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	from := time.Date(2026, 8, 28, 6, 46, 0, 0, time.UTC)
	files := []flashbackWALFile{
		{Name: "000000010000000000000001", Size: 100, Modification: old, Source: "live"},
		{Name: "000000010000000000000002", Size: 100, Modification: old, Source: "live"},
	}
	picked, _, _ := flashbackSelectWAL(files, from, time.Now(), 1<<30, "000000010000000000000002")
	if len(picked) != 2 {
		t.Fatalf("want current+prev despite stale mtime, got %d", len(picked))
	}
}

func TestFlashbackSelectWALClipsOldLiveAndRecycled(t *testing.T) {
	old := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	from := time.Date(2026, 8, 28, 6, 46, 0, 0, time.UTC)
	cur := "00000001000000000000000A"
	var files []flashbackWALFile
	for i := 1; i <= 12; i++ {
		files = append(files, flashbackWALFile{
			Name: fmt.Sprintf("0000000100000000000000%02X", i), Size: 16 << 20, Modification: old, Source: "live",
		})
	}
	picked, total, _ := flashbackSelectWAL(files, from, time.Now(), 1<<30, cur)
	if len(picked) != 2 {
		t.Fatalf("want 2 (prev+current), got %d names=%v", len(picked), walNames(picked))
	}
	if picked[0].Name != "000000010000000000000009" || picked[1].Name != cur {
		t.Fatalf("got %v", walNames(picked))
	}
	if total != 2*(16<<20) {
		t.Fatalf("total %d", total)
	}
	// 回收段（current 之后）不得入选
	for _, f := range picked {
		if f.Name > cur {
			t.Fatalf("recycled %s", f.Name)
		}
	}
}

func TestFlashbackSelectWALPreciseRequiresCheckpoint(t *testing.T) {
	now := time.Now()
	files := []flashbackWALFile{
		{Name: "000000010000000000000001", Size: 10, Modification: now, Source: "archive"},
		{Name: "000000010000000000000002", Size: 10, Modification: now, Source: "live"},
		{Name: "000000010000000000000003", Size: 10, Modification: now, Source: "live"},
	}
	picked, _, _, ok := flashbackSelectWALPrecise(files, now.Add(-time.Hour), now, 1<<30, "000000010000000000000003", "000000010000000000000001")
	if !ok || len(picked) < 2 || picked[0].Name != "000000010000000000000001" {
		t.Fatalf("ok=%v names=%v", ok, walNames(picked))
	}
	_, _, _, ok = flashbackSelectWALPrecise(files, now.Add(-time.Hour), now, 1<<30, "000000010000000000000003", "000000010000000000000099")
	if ok {
		t.Fatal("missing checkpoint must fail")
	}
}

func TestFlashbackSelectWALKeepsNewestOnCap(t *testing.T) {
	now := time.Now()
	files := []flashbackWALFile{
		{Name: "000000010000000000000001", Size: 80, Modification: now, Source: "live"},
		{Name: "000000010000000000000002", Size: 80, Modification: now, Source: "live"},
		{Name: "000000010000000000000003", Size: 80, Modification: now, Source: "live"},
	}
	picked, total, trunc := flashbackSelectWAL(files, now.Add(-time.Hour), now, 160, "000000010000000000000003")
	if !trunc || len(picked) != 2 || total != 160 {
		t.Fatalf("trunc=%v n=%d total=%d names=%v", trunc, len(picked), total, walNames(picked))
	}
	if picked[0].Name != "000000010000000000000002" || picked[1].Name != "000000010000000000000003" {
		t.Fatalf("should keep newest: %v", walNames(picked))
	}
}

func walNames(files []flashbackWALFile) []string {
	out := make([]string, len(files))
	for i, f := range files {
		out[i] = f.Name
	}
	return out
}

func TestFlashbackFPWCacheEvict(t *testing.T) {
	c := flashbackNewFPWCache(2)
	c.Set(1, make([]byte, flashbackXLogPageSize))
	c.Set(2, make([]byte, flashbackXLogPageSize))
	c.Set(3, make([]byte, flashbackXLogPageSize))
	if c.Len() != 2 {
		t.Fatalf("want 2 cached pages, got %d", c.Len())
	}
	if len(c.Get(1)) != 0 {
		t.Fatal("expected oldest key evicted")
	}
	if len(c.Get(2)) != flashbackXLogPageSize || len(c.Get(3)) != flashbackXLogPageSize {
		t.Fatal("expected newer keys kept")
	}
}

func TestFlashbackParseMaxChangesAndDeleteAfter(t *testing.T) {
	rel := &flashbackRelation{
		Schema: "public", Name: "tbl_test", RelNode: 24740, DBOID: 24622,
		PKCols: []string{"id"},
		Columns: []flashbackColumn{
			{Name: "id", AttNum: 1, TypeName: "int4", Typlen: 4, Typalign: "i", IsPK: true},
			{Name: "cname", AttNum: 2, TypeName: "text", Typlen: -1, Typalign: "i"},
		},
	}
	dict := &flashbackDictionary{
		DBOID:     24622,
		ByRelNode: map[uint32]*flashbackRelation{24740: rel},
		Wanted:    map[string]*flashbackRelation{"public.tbl_test": rel},
	}
	rec1 := flashbackTestInsertRecord(7, 24622, 24740, 2, "b", make([]byte, 8100))
	rec2 := flashbackTestInsertRecord(8, 24622, 24740, 9, "z", nil)
	first := flashbackXLogPageSize - 40
	page0 := make([]byte, flashbackXLogPageSize)
	binary.LittleEndian.PutUint16(page0[0:2], 0xD110)
	binary.LittleEndian.PutUint16(page0[2:4], xlpLongHeader)
	copy(page0[40:], rec1[:first])
	rem := len(rec1) - first
	skip := flashbackMaxAlign(rem)
	page1 := make([]byte, flashbackXLogPageSize)
	binary.LittleEndian.PutUint16(page1[0:2], 0xD110)
	binary.LittleEndian.PutUint16(page1[2:4], xlpFirstContRecord)
	binary.LittleEndian.PutUint32(page1[16:20], uint32(rem))
	copy(page1[24:], rec1[first:])
	copy(page1[24+skip:], rec2)
	dir := t.TempDir()
	name := filepath.Join(dir, "000000010000000000000001")
	if err := os.WriteFile(name, append(page0, page1...), 0o600); err != nil {
		t.Fatal(err)
	}
	ch, st, err := flashbackParseWALDirOpts(dir, dict, 24622, flashbackParseOpts{MaxChanges: 1, DeleteAfter: true})
	if err != nil {
		t.Fatal(err)
	}
	if !st.ChangeTrunc {
		t.Fatal("expected change truncation")
	}
	if len(ch) != 1 {
		t.Fatalf("want 1 change, got %d stats=%s", len(ch), st.String())
	}
	if _, err := os.Stat(name); !os.IsNotExist(err) {
		t.Fatalf("expected wal segment deleted, err=%v", err)
	}
}

func TestFlashbackStreamWALFetchParseDelete(t *testing.T) {
	rel := &flashbackRelation{
		Schema: "public", Name: "tbl_test", RelNode: 24740, DBOID: 24622,
		PKCols: []string{"id"},
		Columns: []flashbackColumn{
			{Name: "id", AttNum: 1, TypeName: "int4", Typlen: 4, Typalign: "i", IsPK: true},
			{Name: "cname", AttNum: 2, TypeName: "text", Typlen: -1, Typalign: "i"},
		},
	}
	dict := &flashbackDictionary{
		DBOID:     24622,
		ByRelNode: map[uint32]*flashbackRelation{24740: rel},
		Wanted:    map[string]*flashbackRelation{"public.tbl_test": rel},
	}
	writePage := func(id int32, name string) []byte {
		rec := flashbackTestInsertRecord(7, 24622, 24740, id, name, nil)
		page := make([]byte, flashbackXLogPageSize)
		binary.LittleEndian.PutUint16(page[0:2], 0xD110)
		binary.LittleEndian.PutUint16(page[2:4], xlpLongHeader)
		copy(page[40:], rec)
		return page
	}
	archive := t.TempDir()
	dest := t.TempDir()
	n1, n2 := "000000010000000000000001", "000000010000000000000002"
	if err := os.WriteFile(filepath.Join(archive, n1), writePage(1, "a"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(archive, n2), writePage(2, "b"), 0o600); err != nil {
		t.Fatal(err)
	}
	files := []flashbackWALFile{
		{Name: n1, Size: flashbackXLogPageSize, Source: "archive"},
		{Name: n2, Size: flashbackXLogPageSize, Source: "archive"},
	}
	var got []flashbackChange
	st, written, err := flashbackStreamWAL(context.Background(), nil, dest, archive, files, dict, 24622,
		flashbackParseOpts{DeleteAfter: true}, nil, nil, func(ch flashbackChange) bool {
			got = append(got, ch)
			return true
		})
	if err != nil {
		t.Fatal(err)
	}
	if written != 2*int64(flashbackXLogPageSize) {
		t.Fatalf("written %d", written)
	}
	if len(got) != 2 || got[0].New["id"] != "1" || got[1].New["id"] != "2" {
		t.Fatalf("got %+v stats=%s", got, st.String())
	}
	ents, err := os.ReadDir(dest)
	if err != nil {
		t.Fatal(err)
	}
	if len(ents) != 0 {
		t.Fatalf("expected dest empty after delete-after, got %d", len(ents))
	}
}

func TestFlashbackCleanupOrphanWorkDirs(t *testing.T) {
	base := t.TempDir()
	if err := os.MkdirAll(filepath.Join(base, "task-a", "wal"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(base, "task-b"), 0o700); err != nil {
		t.Fatal(err)
	}
	n, err := flashbackCleanupOrphanWorkDirs(base)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("want 2 cleaned, got %d", n)
	}
	ents, err := os.ReadDir(base)
	if err != nil {
		t.Fatal(err)
	}
	if len(ents) != 0 {
		t.Fatalf("expected empty base, got %d", len(ents))
	}
	if _, err := flashbackCleanupOrphanWorkDirs("/"); err == nil {
		t.Fatal("expected refuse root")
	}
}

func TestFlashbackCountNeedleInReader(t *testing.T) {
	needle := []byte{0xAA, 0xBB}
	r := bytes.NewReader([]byte{0x00, 0xAA, 0xBB, 0x01, 0xAA, 0xBB})
	if n := flashbackCountNeedleInReader(r, needle, make([]byte, 3)); n != 2 {
		t.Fatalf("got %d", n)
	}
}

func TestFlashbackCopyFileStream(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "a")
	dst := filepath.Join(dir, "b")
	if err := os.WriteFile(src, []byte("hello-wal"), 0o600); err != nil {
		t.Fatal(err)
	}
	n, err := flashbackCopyFileStream(src, dst)
	if err != nil || n != 9 {
		t.Fatalf("n=%d err=%v", n, err)
	}
	got, err := os.ReadFile(dst)
	if err != nil || string(got) != "hello-wal" {
		t.Fatalf("got %q", got)
	}
}

func TestFlashbackPublicErrorHidesPQPrefix(t *testing.T) {
	got := flashbackPublicErrorMessage(`pq: invalid byte sequence for encoding "UTF8": 0xbe`)
	if strings.Contains(got, "pq:") {
		t.Fatalf("mysql task must not surface pq: %q", got)
	}
	if !strings.Contains(got, "写入平台任务记录失败") || !strings.Contains(got, "不是目标实例报错") {
		t.Fatalf("should explain hub store, got %q", got)
	}
	wrapped := flashbackPublicError(fmt.Errorf("写入平台闪回 SQL 失败：%w", fmt.Errorf(`pq: invalid byte sequence for encoding "UTF8": 0xbe`)))
	if strings.Contains(wrapped, "pq:") {
		t.Fatalf("wrapped pq must be rewritten: %q", wrapped)
	}
	plain := flashbackPublicErrorMessage("pq: password authentication failed")
	if plain != "写入平台任务记录失败：password authentication failed" {
		t.Fatalf("generic pq: %q", plain)
	}
	keep := "没有可 DUMP 的 binlog 文件"
	if flashbackPublicErrorMessage(keep) != keep {
		t.Fatalf("plain mysql error should stay: %q", flashbackPublicErrorMessage(keep))
	}
	already := "写入平台任务记录失败：生成的闪回 SQL 含非法 UTF-8 字节，不是目标实例报错"
	if flashbackPublicErrorMessage(already) != already {
		t.Fatalf("already rewritten should stay: %q", flashbackPublicErrorMessage(already))
	}
	dump := flashbackPublicErrorMessage("DUMP/解析 binlog: io.CopyN failed. err unexpected EOF, copied 0, expected 106: connection was bad")
	if dump != flashbackMySQLDumpBrokenHint {
		t.Fatalf("dump eof: %q", dump)
	}
}

func TestFlashbackMySQLDumpTransient(t *testing.T) {
	if !flashbackMySQLDumpTransient(fmt.Errorf("io.CopyN failed. err unexpected EOF, copied 0, expected 106: connection was bad")) {
		t.Fatal("eof should be transient")
	}
	if flashbackMySQLDumpTransient(fmt.Errorf("access denied")) {
		t.Fatal("auth should not retry")
	}
}

func TestFlashbackResolveHubDomain_UsesHubID(t *testing.T) {
	taskID := "01aaaaaaaaaaaaaaaaaaaaaaaa"
	hubID := "019fb000-0000-7000-8000-00aaaaaa0001"
	row := &flashback.TaskRow{ID: taskID, InstanceID: hubID, MDMInstanceID: "mdm-1", Tables: "[]"}
	got := flashbackResolveHubDomainWith(row, flashbackHubDomainLookup{
		GetByID: func(id string) *flashbackHubDomain {
			if id == hubID {
				return &flashbackHubDomain{ID: hubID, MDMInstanceID: "mdm-1"}
			}
			return nil
		},
	})
	if got.InstanceID != hubID || got.InstanceID == taskID {
		t.Fatalf("instance_id=%q want hub %q != task %q", got.InstanceID, hubID, taskID)
	}
	if got.MDMInstanceID != "mdm-1" {
		t.Fatalf("mdm=%q", got.MDMInstanceID)
	}
	task := flashbackTaskFromResolved(row, got)
	if task.InstanceID != hubID || task.DomainInstanceID != hubID || task.ID == task.InstanceID {
		t.Fatalf("dto id=%q instance=%q domain=%q", task.ID, task.InstanceID, task.DomainInstanceID)
	}
}

func TestFlashbackResolveHubDomain_RejectsTaskID(t *testing.T) {
	taskID := "01aaaaaaaaaaaaaaaaaaaaaaaa"
	row := &flashback.TaskRow{ID: taskID, InstanceID: taskID, Tables: "[]"}
	got := flashbackResolveHubDomainWith(row, flashbackHubDomainLookup{})
	if got.InstanceID != "" {
		t.Fatalf("must not echo task id as instance_id: %q", got.InstanceID)
	}
	if got.Warning == "" {
		t.Fatal("expected warning when stored id equals task id")
	}
	task := flashbackTaskFromResolved(row, got)
	if task.InstanceID != "" || task.DomainInstanceID != "" {
		t.Fatalf("dto leaked task id: instance=%q domain=%q", task.InstanceID, task.DomainInstanceID)
	}
}

func TestFlashbackResolveHubDomain_HostPort(t *testing.T) {
	taskID := "01aaaaaaaaaaaaaaaaaaaaaaaa"
	hubID := "019fb000-0000-7000-8000-00aaaaaa0001"
	row := &flashback.TaskRow{
		ID: taskID, InstanceID: "mysql.test.example",
		Host: "10.1.1.1", Port: 3306, Tables: "[]",
	}
	got := flashbackResolveHubDomainWith(row, flashbackHubDomainLookup{
		ListByHostPort: func(host string, port int) []*flashbackHubDomain {
			if host == "10.1.1.1" && port == 3306 {
				return []*flashbackHubDomain{{ID: hubID, MDMInstanceID: "mysql.test.example"}}
			}
			return nil
		},
	})
	if got.InstanceID != hubID || got.InstanceID == taskID {
		t.Fatalf("host/port resolve: got %q want %q", got.InstanceID, hubID)
	}
	if !got.Changed {
		t.Fatal("expected Changed when host/port replaces stored hostname")
	}
}

func TestFlashbackResolveHubDomain_MDM(t *testing.T) {
	taskID := "01aaaaaaaaaaaaaaaaaaaaaaaa"
	hubID := "019fb000-0000-7000-8000-00aaaaaa0001"
	row := &flashback.TaskRow{ID: taskID, InstanceID: "mdm-res-1", MDMInstanceID: "mdm-res-1", Tables: "[]"}
	got := flashbackResolveHubDomainWith(row, flashbackHubDomainLookup{
		ListByMDM: func(mdmID string) []*flashbackHubDomain {
			if mdmID == "mdm-res-1" {
				return []*flashbackHubDomain{{ID: hubID, MDMInstanceID: "mdm-res-1"}}
			}
			return nil
		},
	})
	if got.InstanceID != hubID {
		t.Fatalf("mdm resolve: got %q want %q", got.InstanceID, hubID)
	}
}

func TestFlashbackResolveHubDomain_KeepStoredWhenUnresolved(t *testing.T) {
	taskID := "01aaaaaaaaaaaaaaaaaaaaaaaa"
	stored := "019fb000-0000-7000-8000-00aaaaaa0001"
	row := &flashback.TaskRow{ID: taskID, InstanceID: stored, Tables: "[]"}
	got := flashbackResolveHubDomainWith(row, flashbackHubDomainLookup{})
	if got.InstanceID != stored {
		t.Fatalf("keep stored when not task id: got %q", got.InstanceID)
	}
	if got.Warning == "" {
		t.Fatal("expected warning when lookup misses")
	}
	if got.Changed {
		t.Fatal("should not persist when merely warning on unmatched stored hub-shaped id")
	}
}

func TestFlashbackTaskFromRow_NeverEqualsTaskID(t *testing.T) {
	row := &flashback.TaskRow{ID: "same-id", InstanceID: "same-id", MDMInstanceID: "same-id", Tables: "[]"}
	got := flashbackTaskFromRow(row)
	if got.InstanceID != "" || got.DomainInstanceID != "" || got.MDMInstanceID != "" {
		t.Fatalf("mapper leaked task id: %+v", got)
	}
}

func TestFlashbackBindTaskHubDomain_RegeneratesTaskID(t *testing.T) {
	hub := "019fb000-0000-7000-8000-00aaaaaa0001"
	row := &flashback.TaskRow{ID: hub}
	if err := flashbackBindTaskHubDomain(row, hub, "mdm-x"); err != nil {
		t.Fatal(err)
	}
	if row.ID == hub {
		t.Fatal("task id must be regenerated when it equals hub id")
	}
	if row.InstanceID != hub || row.MDMInstanceID != "mdm-x" {
		t.Fatalf("bound ids: instance=%q mdm=%q", row.InstanceID, row.MDMInstanceID)
	}
}

func TestFlashbackStagePercent(t *testing.T) {
	if got := flashbackStagePercent(0, 0); got != 0 {
		t.Fatalf("0/0=%d", got)
	}
	if got := flashbackStagePercent(3, 31); got != 9 {
		t.Fatalf("3/31=%d want 9", got)
	}
	if got := flashbackStagePercent(31, 31); got != 100 {
		t.Fatalf("31/31=%d", got)
	}
}

func TestFlashbackProgressFromRow(t *testing.T) {
	running := &flashback.TaskRow{
		Status: flashback.StatusRunning, LogDone: 3, LogTotal: 31, ParseDone: 2, ParseTotal: 31,
	}
	p := flashbackProgressFromRow(running)
	if p.Phase != "parse" || p.FetchPercent != 9 || p.ParsePercent != 6 || p.FetchRemain != 28 || p.ParseRemain != 29 {
		t.Fatalf("running: %+v", p)
	}
	fetching := &flashback.TaskRow{
		Status: flashback.StatusRunning, LogDone: 2, LogTotal: 31, ParseDone: 2, ParseTotal: 31,
	}
	if got := flashbackProgressFromRow(fetching); got.Phase != "fetch_logs" {
		t.Fatalf("fetching phase=%q", got.Phase)
	}
	done := &flashback.TaskRow{Status: flashback.StatusSucceeded, LogDone: 30, LogTotal: 31, ParseDone: 30, ParseTotal: 31}
	dp := flashbackProgressFromRow(done)
	if dp.Phase != "done" || dp.FetchPercent != 100 || dp.ParsePercent != 100 || dp.FetchDone != 31 || dp.ParseDone != 31 {
		t.Fatalf("done: %+v", dp)
	}
	failed := &flashback.TaskRow{Status: flashback.StatusFailed, LogDone: 3, LogTotal: 31, ParseDone: 2, ParseTotal: 31}
	fp := flashbackProgressFromRow(failed)
	if fp.Phase != "failed" || fp.FetchPercent != 9 {
		t.Fatalf("failed: %+v", fp)
	}
	task := flashbackTaskFromRow(running)
	if task.Progress == nil || task.Progress.FetchTotal != 31 {
		t.Fatalf("dto progress=%+v", task.Progress)
	}
}

func TestFlashbackMySQLRangeFileCount(t *testing.T) {
	logs := []flashbackMySQLBinlogFile{{Name: "mysql-bin.0001"}, {Name: "mysql-bin.0002"}, {Name: "mysql-bin.0003"}}
	if got := flashbackMySQLRangeFileCount(logs, "mysql-bin.0002", "mysql-bin.0003"); got != 2 {
		t.Fatalf("count=%d", got)
	}
	if got := flashbackMySQLRangeFileIndex(logs, "mysql-bin.0002", "mysql-bin.0003"); got != 2 {
		t.Fatalf("index=%d", got)
	}
	if got := flashbackMySQLRangeFileCount(nil, "", ""); got != 1 {
		t.Fatalf("empty count=%d", got)
	}
}

func TestFlashbackMySQLDumpRemainBytes(t *testing.T) {
	logs := []flashbackMySQLBinlogFile{
		{Name: "mysql-bin.0001", Size: 1000},
		{Name: "mysql-bin.0002", Size: 2000},
		{Name: "mysql-bin.0003", Size: 4000},
	}
	if got := flashbackMySQLDumpRemainBytes(logs, "mysql-bin.0002", 500, "mysql-bin.0003", 1000); got != 1500+1000 {
		t.Fatalf("span=%d", got)
	}
	if got := flashbackMySQLDumpRemainBytes(logs, "mysql-bin.0002", 4, "mysql-bin.0002", 504); got != 500 {
		t.Fatalf("same file=%d", got)
	}
	if got := flashbackMySQLDumpReadBytes(logs, "mysql-bin.0002", 500, "mysql-bin.0002", 1500); got != 1000 {
		t.Fatalf("read=%d", got)
	}
}

func TestFlashbackMySQLByteProgress(t *testing.T) {
	if d, tot := flashbackMySQLByteProgress(0, 1000, false); d != 0 || tot != 1000 {
		t.Fatalf("zero %d/%d", d, tot)
	}
	if d, tot := flashbackMySQLByteProgress(450, 1000, false); d != 450 || tot != 1000 {
		t.Fatalf("mid %d/%d", d, tot)
	}
	if d, tot := flashbackMySQLByteProgress(1000, 1000, false); d != 999 || tot != 1000 {
		t.Fatalf("cap before finish %d/%d", d, tot)
	}
	if d, tot := flashbackMySQLByteProgress(1000, 1000, true); d != 1000 || tot != 1000 {
		t.Fatalf("finish %d/%d", d, tot)
	}
	if got := flashbackFormatBytes(1536); got != "1.5KB" {
		t.Fatalf("fmt=%s", got)
	}
	if flashbackStageRemain(450, 1000) != 550 {
		t.Fatalf("remain")
	}
}

func TestFlashbackClampStage(t *testing.T) {
	if got := flashbackClampStage(-1, 10); got != 0 {
		t.Fatalf("neg=%d", got)
	}
	if got := flashbackClampStage(12, 10); got != 10 {
		t.Fatalf("over=%d", got)
	}
}
