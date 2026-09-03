package service

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
)

// flashbackChange 一条已解码的行级变更。
type flashbackChange struct {
	XID      uint32
	XID64    int64 // MySQL XID_EVENT（64 位）；PG 仍用 XID
	TS       time.Time
	Schema   string
	Table    string
	Op       string // INSERT / UPDATE / DELETE / CREATE / DROP / ALTER / TRUNCATE
	Old      map[string]string
	New      map[string]string
	PKCols   []string
	NoPK     bool
	RelNode  uint32
	DDLRedo  string
	DDLUndo  string
	DDLRisk  string
	MySQL    bool
	Block    uint32
	Offnum   uint16
	NewBlock uint32
	NewOff   uint16
}

func (ch flashbackChange) ident(name string) string {
	if ch.MySQL {
		return "`" + strings.ReplaceAll(name, "`", "``") + "`"
	}
	return flashbackQuoteIdent(name)
}

func (ch flashbackChange) qual() string {
	if ch.MySQL {
		if strings.TrimSpace(ch.Schema) == "" {
			return ch.ident(ch.Table)
		}
		return ch.ident(ch.Schema) + "." + ch.ident(ch.Table)
	}
	return flashbackQualified(ch.Schema, ch.Table)
}

func flashbackIsDroppedColName(name string) bool {
	return strings.Contains(name, "pg.dropped")
}

func flashbackQuoteIdent(name string) string {
	name = strings.ReplaceAll(name, `"`, `""`)
	return `"` + name + `"`
}

func flashbackQuoteLiteral(v string) string {
	if v == `\N` || strings.EqualFold(v, "NULL") {
		return "NULL"
	}
	if strings.HasPrefix(v, `\RAW:`) {
		return strings.TrimPrefix(v, `\RAW:`)
	}
	return "'" + strings.ReplaceAll(v, `'`, `''`) + "'"
}

func flashbackQualified(schema, table string) string {
	if strings.TrimSpace(schema) == "" {
		schema = "public"
	}
	return flashbackQuoteIdent(schema) + "." + flashbackQuoteIdent(table)
}

func flashbackSortedKeys(vals map[string]string) []string {
	keys := make([]string, 0, len(vals))
	for k := range vals {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func flashbackWhere(cols []string, vals map[string]string) (string, string) {
	return flashbackWhereIdent(cols, vals, flashbackQuoteIdent)
}

func flashbackWhereIdent(cols []string, vals map[string]string, ident func(string) string) (string, string) {
	if ident == nil {
		ident = flashbackQuoteIdent
	}
	if len(cols) == 0 {
		cols = flashbackSortedKeys(vals)
	}
	if len(cols) == 0 {
		return "", "无可用 WHERE 列"
	}
	var parts []string
	for _, c := range cols {
		if flashbackIsDroppedColName(c) {
			continue
		}
		v, ok := vals[c]
		if !ok {
			parts = append(parts, ident(c)+" IS NULL")
			continue
		}
		if v == `\N` || strings.EqualFold(v, "NULL") {
			parts = append(parts, ident(c)+" IS NULL")
			continue
		}
		parts = append(parts, ident(c)+" = "+flashbackQuoteLiteral(v))
	}
	risk := ""
	if len(cols) > 4 {
		risk = "WHERE 使用多列等值，请人工核对"
	}
	return strings.Join(parts, " AND "), risk
}

func flashbackCTIDLiteral(block uint32, off uint16) string {
	if off == 0 {
		return ""
	}
	return fmt.Sprintf("'(%d,%d)'", block, off)
}

func flashbackAppendCTID(where, risk string, ch flashbackChange, newSide bool) (string, string) {
	if ch.MySQL || !ch.NoPK {
		return where, risk
	}
	lit := flashbackCTIDLiteral(ch.Block, ch.Offnum)
	if newSide {
		lit = flashbackCTIDLiteral(ch.NewBlock, ch.NewOff)
		if lit == "" {
			lit = flashbackCTIDLiteral(ch.Block, ch.Offnum)
		}
	}
	if lit == "" || where == "" {
		return where, risk
	}
	return where + " AND ctid = " + lit, joinRisk(risk, "ctid 为变更当时值，VACUUM 后可能失效")
}

func flashbackSetClause(vals map[string]string, skip map[string]struct{}) string {
	return flashbackSetClauseIdent(vals, skip, flashbackQuoteIdent)
}

func flashbackSetClauseIdent(vals map[string]string, skip map[string]struct{}, ident func(string) string) string {
	if ident == nil {
		ident = flashbackQuoteIdent
	}
	var parts []string
	for _, k := range flashbackSortedKeys(vals) {
		if flashbackIsDroppedColName(k) {
			continue
		}
		if _, ok := skip[k]; ok {
			continue
		}
		parts = append(parts, ident(k)+" = "+flashbackQuoteLiteral(vals[k]))
	}
	return strings.Join(parts, ", ")
}

func flashbackInsertSQL(schema, table string, vals map[string]string) string {
	return flashbackInsertSQLIdent(schema, table, vals, flashbackQuoteIdent, false)
}

func flashbackInsertSQLIdent(schema, table string, vals map[string]string, ident func(string) string, mysql bool) string {
	if ident == nil {
		ident = flashbackQuoteIdent
	}
	var cols, lits []string
	for _, k := range flashbackSortedKeys(vals) {
		if flashbackIsDroppedColName(k) {
			continue
		}
		cols = append(cols, ident(k))
		lits = append(lits, flashbackQuoteLiteral(vals[k]))
	}
	qual := flashbackQualified(schema, table)
	if mysql {
		ch := flashbackChange{Schema: schema, Table: table, MySQL: true}
		qual = ch.qual()
	}
	return fmt.Sprintf("INSERT INTO %s (%s) VALUES (%s);",
		qual, strings.Join(cols, ", "), strings.Join(lits, ", "))
}

func flashbackUndoSQL(ch flashbackChange) (stmt string, risk string) {
	ident := ch.ident
	switch strings.ToUpper(ch.Op) {
	case "INSERT":
		where, r := flashbackWhereIdent(ch.PKCols, ch.New, ident)
		if where == "" {
			return "", "INSERT 无法生成 DELETE：缺少新行值"
		}
		if ch.NoPK {
			r = joinRisk(r, "无主键，DELETE 使用全列等值")
		}
		where, r = flashbackAppendCTID(where, r, ch, true)
		return fmt.Sprintf("DELETE FROM %s WHERE %s;", ch.qual(), where), r
	case "DELETE":
		if len(ch.Old) == 0 {
			if ch.MySQL {
				return "", "DELETE 无法生成 INSERT：binlog 未包含旧行（需要 binlog_row_image=FULL）"
			}
			return "", "DELETE 无法生成 INSERT：WAL 未包含旧行（需要 REPLICA IDENTITY FULL 或 FPW）"
		}
		return flashbackInsertSQLIdent(ch.Schema, ch.Table, ch.Old, ident, ch.MySQL), ""
	case "UPDATE":
		if len(ch.Old) == 0 {
			if ch.MySQL {
				return "", "UPDATE 无法生成逆向 UPDATE：binlog 未包含旧行（需要 binlog_row_image=FULL）"
			}
			return "", "UPDATE 无法生成逆向 UPDATE：WAL 未包含旧行"
		}
		whereSrc := ch.New
		if len(whereSrc) == 0 {
			whereSrc = ch.Old
		}
		pk := ch.PKCols
		if len(pk) == 0 {
			pk = flashbackSortedKeys(whereSrc)
			risk = "无主键，UPDATE WHERE 使用全列等值"
		}
		where, r := flashbackWhereIdent(pk, whereSrc, ident)
		risk = joinRisk(risk, r)
		where, risk = flashbackAppendCTID(where, risk, ch, true)
		set := flashbackSetClauseIdent(ch.Old, nil, ident)
		if set == "" || where == "" {
			return "", "UPDATE 无法生成 SET/WHERE"
		}
		return fmt.Sprintf("UPDATE %s SET %s WHERE %s;", ch.qual(), set, where), risk
	case "CREATE", "DROP", "ALTER", "TRUNCATE":
		return ch.DDLUndo, ch.DDLRisk
	default:
		return "", "未知操作 " + ch.Op
	}
}

func flashbackRedoSQL(ch flashbackChange) (stmt string, risk string) {
	ident := ch.ident
	switch strings.ToUpper(ch.Op) {
	case "INSERT":
		if len(ch.New) == 0 {
			return "", "INSERT 缺少新行"
		}
		return flashbackInsertSQLIdent(ch.Schema, ch.Table, ch.New, ident, ch.MySQL), ""
	case "DELETE":
		where, r := flashbackWhereIdent(ch.PKCols, ch.Old, ident)
		if where == "" {
			return "", "DELETE 无法生成 WHERE"
		}
		if ch.NoPK {
			r = joinRisk(r, "无主键，DELETE 使用全列等值")
		}
		where, r = flashbackAppendCTID(where, r, ch, false)
		return fmt.Sprintf("DELETE FROM %s WHERE %s;", ch.qual(), where), r
	case "UPDATE":
		if len(ch.New) == 0 {
			return "", "UPDATE 缺少新行"
		}
		pk := ch.PKCols
		src := ch.Old
		if len(src) == 0 {
			src = ch.New
		}
		if len(pk) == 0 {
			pk = flashbackSortedKeys(src)
			risk = "无主键，UPDATE WHERE 使用全列等值"
		}
		where, r := flashbackWhereIdent(pk, src, ident)
		risk = joinRisk(risk, r)
		where, risk = flashbackAppendCTID(where, risk, ch, false)
		set := flashbackSetClauseIdent(ch.New, nil, ident)
		return fmt.Sprintf("UPDATE %s SET %s WHERE %s;", ch.qual(), set, where), risk
	case "CREATE", "DROP", "ALTER", "TRUNCATE":
		return ch.DDLRedo, ch.DDLRisk
	default:
		return "", "未知操作 " + ch.Op
	}
}

func joinRisk(a, b string) string {
	a = strings.TrimSpace(a)
	b = strings.TrimSpace(b)
	if a == "" {
		return b
	}
	if b == "" {
		return a
	}
	return a + "；" + b
}

const flashbackAllTablesWarnMin = 100

// flashbackNormalizeTableNames 去掉空白项。结果为空表示整库。
func flashbackNormalizeTableNames(tables []string) []string {
	out := make([]string, 0, len(tables))
	for _, t := range tables {
		t = strings.TrimSpace(t)
		if t == "" {
			continue
		}
		out = append(out, t)
	}
	return out
}

func flashbackTablesIsAll(tables []string) bool {
	return len(flashbackNormalizeTableNames(tables)) == 0
}

func flashbackTablesJSON(tables []string) string {
	names := flashbackNormalizeTableNames(tables)
	if names == nil {
		names = []string{}
	}
	raw, err := json.Marshal(names)
	if err != nil || len(raw) == 0 || string(raw) == "null" {
		return "[]"
	}
	return string(raw)
}

func flashbackParseTableName(raw string) (schema, table string, err error) {
	raw = strings.TrimSpace(raw)
	raw = strings.Trim(raw, `"`)
	if raw == "" {
		return "", "", fmt.Errorf("empty table name")
	}
	parts := strings.Split(raw, ".")
	switch len(parts) {
	case 1:
		return "public", strings.Trim(parts[0], `"`), nil
	case 2:
		return strings.Trim(parts[0], `"`), strings.Trim(parts[1], `"`), nil
	default:
		return "", "", fmt.Errorf("invalid table %q, expect schema.table", raw)
	}
}

func flashbackNormalizeSQLTypes(raw string) map[string]struct{} {
	out := map[string]struct{}{}
	for _, p := range strings.Split(raw, ",") {
		p = strings.ToUpper(strings.TrimSpace(p))
		if p == "" {
			continue
		}
		out[p] = struct{}{}
	}
	return out
}

func flashbackChangeXID(ch flashbackChange) int64 {
	if ch.XID64 != 0 {
		return ch.XID64
	}
	return int64(ch.XID)
}

func flashbackIsDDLOp(op string) bool {
	switch strings.ToUpper(strings.TrimSpace(op)) {
	case "CREATE", "DROP", "ALTER", "TRUNCATE", "RENAME":
		return true
	}
	return false
}

func flashbackSQLIsDDLStatement(stmt string) bool {
	u := strings.ToUpper(strings.TrimSpace(stmt))
	for _, p := range []string{"CREATE ", "DROP ", "ALTER ", "TRUNCATE ", "RENAME "} {
		if strings.HasPrefix(u, p) {
			return true
		}
	}
	return false
}

func flashbackWantOp(filter map[string]struct{}, op string) bool {
	if len(filter) == 0 {
		return true
	}
	op = strings.ToUpper(strings.TrimSpace(op))
	if _, ok := filter[op]; ok {
		return true
	}
	if _, ok := filter["DDL"]; ok {
		switch op {
		case "CREATE", "DROP", "ALTER", "TRUNCATE":
			return true
		}
	}
	return false
}

// flashbackSQLPreviewKind 输出类型 → 预览 kind。queryKind 非空时覆盖任务 output_kind。
func flashbackSQLPreviewKind(outputKind, queryKind string) string {
	q := strings.ToLower(strings.TrimSpace(queryKind))
	switch q {
	case "undo", "flashback":
		return "undo"
	case "redo", "original":
		return "redo"
	}
	if strings.EqualFold(strings.TrimSpace(outputKind), "original") {
		return "redo"
	}
	return "undo"
}

// flashbackSQLPreviewOps 把 sql_type 展开为 WAL 原始 op。空表示不限。
func flashbackSQLPreviewOps(sqlType string) []string {
	f := flashbackNormalizeSQLTypes(sqlType)
	if len(f) == 0 {
		return nil
	}
	var ops []string
	seen := map[string]struct{}{}
	add := func(op string) {
		if _, ok := seen[op]; ok {
			return
		}
		seen[op] = struct{}{}
		ops = append(ops, op)
	}
	for _, k := range []string{"INSERT", "UPDATE", "DELETE", "DDL", "CREATE", "DROP", "ALTER", "TRUNCATE"} {
		if _, ok := f[k]; !ok {
			continue
		}
		if k == "DDL" {
			add("CREATE")
			add("DROP")
			add("ALTER")
			add("TRUNCATE")
			continue
		}
		add(k)
	}
	for k := range f {
		if k == "DDL" {
			continue
		}
		add(k)
	}
	return ops
}

func flashbackSQLPreviewFilter(outputKind, sqlType, queryKind, querySQLType string) (kind string, ops []string) {
	kind = flashbackSQLPreviewKind(outputKind, queryKind)
	if strings.TrimSpace(querySQLType) != "" {
		sqlType = querySQLType
	}
	return kind, flashbackSQLPreviewOps(sqlType)
}
