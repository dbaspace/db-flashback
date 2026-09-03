package service

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"db-flashback/internal/service/dto"
	"db-flashback/internal/storage/flashback"
)

// flashbackMySQLSelftestMinCols 5.7 起官网类型族（数值/无符号/同义名/位串/字符/国家字符/四档 TEXT·BLOB/日期时间含小数秒/JSON/ENUM/SET/空间）。
// https://dev.mysql.com/doc/refman/8.0/en/data-types.html
const flashbackMySQLSelftestMinCols = 80

func flashbackMySQLIdent(name string) string {
	return "`" + strings.ReplaceAll(name, "`", "``") + "`"
}

func flashbackMySQLParseVersion(ver string) (major, minor int) {
	ver = strings.TrimSpace(ver)
	if i := strings.IndexAny(ver, "- "); i > 0 {
		ver = ver[:i]
	}
	parts := strings.Split(ver, ".")
	if len(parts) > 0 {
		major, _ = strconv.Atoi(parts[0])
	}
	if len(parts) > 1 {
		minor, _ = strconv.Atoi(parts[1])
	}
	return major, minor
}

func flashbackMySQLHasVector(major int) bool {
	return major >= 9
}

func flashbackMySQLSelftestTableSQL(dbName, table string, major int) string {
	ident := flashbackMySQLIdent(dbName) + "." + flashbackMySQLIdent(table)
	extra := ""
	if major >= 8 {
		extra += ",\n  c_cs_utf8mb3 VARCHAR(32) CHARACTER SET utf8mb3"
	}
	if flashbackMySQLHasVector(major) {
		extra += ",\n  c_vector VECTOR(3)"
	}
	return fmt.Sprintf(`
CREATE TABLE %s (
  id INT NOT NULL PRIMARY KEY,
  c_tiny TINYINT,
  c_tiny_u TINYINT UNSIGNED,
  c_tiny1 TINYINT(1),
  c_bool BOOLEAN,
  c_bool2 BOOL,
  c_small SMALLINT,
  c_small_u SMALLINT UNSIGNED,
  c_medium MEDIUMINT,
  c_medium_u MEDIUMINT UNSIGNED,
  c_int INT,
  c_int_u INT UNSIGNED,
  c_integer INTEGER,
  c_integer_u INTEGER UNSIGNED,
  c_int_zf INT(8) ZEROFILL,
  c_big BIGINT,
  c_big_u BIGINT UNSIGNED,
  c_dec DECIMAL(12,2),
  c_dec_u DECIMAL(12,2) UNSIGNED,
  c_dec_syn DEC(10,2),
  c_fixed FIXED(10,2),
  c_numeric NUMERIC(10,4),
  c_numeric_u NUMERIC(10,2) UNSIGNED,
  c_dec38 DECIMAL(38,10),
  c_float FLOAT,
  c_float_u FLOAT UNSIGNED,
  c_float_p FLOAT(10,2),
  c_double DOUBLE,
  c_double_u DOUBLE UNSIGNED,
  c_double_p DOUBLE(16,4),
  c_double_prec DOUBLE PRECISION,
  c_real REAL,
  c_real_u REAL UNSIGNED,
  c_bit BIT(8),
  c_bit1 BIT(1),
  c_bit32 BIT(32),
  c_bit64 BIT(64),
  c_char CHAR(8),
  c_char32 CHAR(32),
  c_varchar VARCHAR(64),
  c_varchar255 VARCHAR(255),
  c_cs_utf8mb4 VARCHAR(32) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin,
  c_cs_utf8 VARCHAR(32) CHARACTER SET utf8,
  c_cs_latin1 VARCHAR(32) CHARACTER SET latin1,
  c_cs_gbk VARCHAR(32) CHARACTER SET gbk,
  c_cs_ascii VARCHAR(32) CHARACTER SET ascii,
  c_cs_binary VARCHAR(32) CHARACTER SET binary,
  c_text_utf8mb4 TEXT CHARACTER SET utf8mb4,
  c_text_utf8 TEXT CHARACTER SET utf8,
  c_text_gbk TEXT CHARACTER SET gbk,
  c_binary BINARY(4),
  c_binary16 BINARY(16),
  c_varbinary VARBINARY(16),
  c_varbinary255 VARBINARY(255),
  c_nchar NCHAR(8),
  c_nvarchar NVARCHAR(32),
  c_natchar NATIONAL CHAR(8),
  c_natvarchar NATIONAL VARCHAR(32),
  c_tinytext TINYTEXT,
  c_text TEXT,
  c_mediumtext MEDIUMTEXT,
  c_longtext LONGTEXT,
  c_tinyblob TINYBLOB,
  c_blob BLOB,
  c_mediumblob MEDIUMBLOB,
  c_longblob LONGBLOB,
  c_enum ENUM('a','b','c'),
  c_enum2 ENUM('small','medium','large'),
  c_set SET('x','y','z'),
  c_set2 SET('red','green','blue'),
  c_date DATE,
  c_time TIME,
  c_time6 TIME(6),
  c_datetime DATETIME,
  c_datetime6 DATETIME(6),
  c_timestamp TIMESTAMP NULL,
  c_timestamp6 TIMESTAMP(6) NULL,
  c_year YEAR,
  c_year2 YEAR,
  c_json JSON,
  c_json2 JSON,
  c_geom GEOMETRY,
  c_point POINT,
  c_linestring LINESTRING,
  c_polygon POLYGON,
  c_multipoint MULTIPOINT,
  c_multilinestring MULTILINESTRING,
  c_multipolygon MULTIPOLYGON,
  c_geomcoll GEOMETRYCOLLECTION
  %s
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`, ident, extra)
}

func flashbackMySQLSelftestInsertSQL(dbName, table string, major int) string {
	ident := flashbackMySQLIdent(dbName) + "." + flashbackMySQLIdent(table)
	cols := `
  id, c_tiny, c_tiny_u, c_tiny1, c_bool, c_bool2, c_small, c_small_u, c_medium, c_medium_u,
  c_int, c_int_u, c_integer, c_integer_u, c_int_zf, c_big, c_big_u,
  c_dec, c_dec_u, c_dec_syn, c_fixed, c_numeric, c_numeric_u, c_dec38,
  c_float, c_float_u, c_float_p, c_double, c_double_u, c_double_p, c_double_prec, c_real, c_real_u,
  c_bit, c_bit1, c_bit32, c_bit64,
  c_char, c_char32, c_varchar, c_varchar255,
  c_cs_utf8mb4, c_cs_utf8, c_cs_latin1, c_cs_gbk, c_cs_ascii, c_cs_binary,
  c_text_utf8mb4, c_text_utf8, c_text_gbk,
  c_binary, c_binary16, c_varbinary, c_varbinary255,
  c_nchar, c_nvarchar, c_natchar, c_natvarchar,
  c_tinytext, c_text, c_mediumtext, c_longtext,
  c_tinyblob, c_blob, c_mediumblob, c_longblob,
  c_enum, c_enum2, c_set, c_set2, c_date, c_time, c_time6, c_datetime, c_datetime6, c_timestamp, c_timestamp6, c_year, c_year2, c_json, c_json2,
  c_geom, c_point, c_linestring, c_polygon, c_multipoint, c_multilinestring, c_multipolygon, c_geomcoll`
	v1 := `1, 7, 200, 1, 1, 1, 11, 40000, 100001, 8000000,
  41, 3000000000, 41, 3000000000, 7, 9999999999, 18000000000000000000,
  123.45, 200.50, 11.10, 12.20, 12.3456, 1.50, 1234567890.1234567890,
  1.25, 3.5, 1.25, 1.5, 4.25, 3.1415, 9.5, 2.75, 2.25,
  b'10101010', b'1', b'10101010101010101010101010101010', b'1010101010101010',
  'pad1', 'char32pad1', 'v1', 'varchar255-1',
  '你好mb4', '你好utf8', 'cafe', '汉字gbk', 'ascii1', 'bin1',
  '文mb4', '文utf8', '文gbk',
  X'DEAD0000', X'DEADBEEFDEADBEEFDEADBEEFDEADBEEF', X'DEADBE', X'CAFE',
  'nch1', 'nv1', 'nc1', 'nvx1',
  'tt1', 'hello', 'mt1', 'lt1',
  X'DE', X'DEAD', X'DEADBEEF', X'DEADBEEF00',
  'a', 'small', 'x', 'red,green', '2024-01-02', '12:00:00', '01:02:03.123456',
  '2024-06-01 12:00:00', '2024-06-01 12:00:00.123456', '2024-06-01 12:00:00', '2024-06-01 12:00:00.123456',
  2024, 2023, '{"k":1}', '{"x":1}',
  ST_GeomFromText('POINT(1 1)'), ST_GeomFromText('POINT(1 2)'),
  ST_GeomFromText('LINESTRING(0 0,1 1)'), ST_GeomFromText('POLYGON((0 0,1 0,1 1,0 1,0 0))'),
  ST_GeomFromText('MULTIPOINT(1 1,2 2)'), ST_GeomFromText('MULTILINESTRING((0 0,1 1),(2 2,3 3))'),
  ST_GeomFromText('MULTIPOLYGON(((0 0,1 0,1 1,0 1,0 0)))'),
  ST_GeomFromText('GEOMETRYCOLLECTION(POINT(1 1),LINESTRING(0 0,1 1))')`
	v2 := `2, 8, 201, 0, 0, 0, 12, 40001, 100002, 8000001,
  42, 3000000001, 42, 3000000001, 8, 1, 18000000000000000001,
  10.00, 88.00, 21.10, 22.20, 8.5000, 8.50, 9876543210.0987654321,
  2.5, 6.5, 2.50, 2.25, 7.25, 6.2800, 8.25, 3.5, 4.25,
  b'11001100', b'0', b'11001100110011001100110011001100', b'1100110011001100',
  'pad2', 'char32pad2', 'v2', 'varchar255-2',
  '世界mb4😀', '世界utf8', 'naiv', '汉字GBK2', 'ascii2', 'bin2',
  '世mb4', '世utf8', '世gbk',
  X'BEEF0000', X'BEEFCAFEBEEFCAFEBEEFCAFEBEEFCAFE', X'BEEFCA', X'BEEF',
  'nch2', 'nv2', 'nc2', 'nvx2',
  'tt2', 'keep', 'mt2', 'lt2',
  X'BE', X'BEEF', X'BEEFCAFE', X'BEEFCAFE00',
  'b', 'large', 'y', 'blue', '2025-12-31', '08:30:00', '08:30:00.654321',
  '2025-03-01 09:00:00', '2025-03-01 09:00:00.654321', '2025-01-15 08:30:00', '2025-01-15 08:30:00.654321',
  2025, 2022, '{"k":2}', '{"x":2}',
  ST_GeomFromText('POINT(3 3)'), ST_GeomFromText('POINT(3 4)'),
  ST_GeomFromText('LINESTRING(2 2,3 3)'), ST_GeomFromText('POLYGON((0 0,2 0,2 2,0 2,0 0))'),
  ST_GeomFromText('MULTIPOINT(3 3,4 4)'), ST_GeomFromText('MULTILINESTRING((1 1,2 2),(3 3,4 4))'),
  ST_GeomFromText('MULTIPOLYGON(((0 0,2 0,2 2,0 2,0 0)))'),
  ST_GeomFromText('GEOMETRYCOLLECTION(POINT(3 3),LINESTRING(2 2,3 3))')`
	if major >= 8 {
		cols += `, c_cs_utf8mb3`
		v1 += `, '你好mb3'`
		v2 += `, '世界mb3'`
	}
	if flashbackMySQLHasVector(major) {
		cols += `, c_vector`
		v1 += `, STRING_TO_VECTOR('[1,2,3]')`
		v2 += `, STRING_TO_VECTOR('[4,5,6]')`
	}
	return fmt.Sprintf("INSERT INTO %s (%s) VALUES\n(%s),\n(%s)", ident, cols, v1, v2)
}

func flashbackMySQLDetectMajor(ctx context.Context, db *sql.DB) int {
	major, _ := flashbackMySQLParseVersion(flashbackMySQLVar(ctx, db, "version"))
	return major
}

const (
	flashbackMySQLSelftestBeforeMark = "before_window"
	flashbackMySQLSelftestAfterMark  = "after_window"
	flashbackMySQLSelftestTick       = 1200 * time.Millisecond
)

func flashbackMySQLSelftestOutsideInsert(ident string, id int, mark string) string {
	return fmt.Sprintf("INSERT INTO %s (id, c_varchar, c_text) VALUES (%d, '%s', '%s')", ident, id, mark, mark)
}

// flashbackMySQLServerNow 取实例 UNIX 时间，与 binlog Header.Timestamp 对齐。
func flashbackMySQLServerNow(ctx context.Context, db *sql.DB) time.Time {
	var sec sql.NullFloat64
	if err := db.QueryRowContext(ctx, "SELECT UNIX_TIMESTAMP(NOW(6))").Scan(&sec); err == nil && sec.Valid && sec.Float64 > 0 {
		whole := int64(sec.Float64)
		frac := sec.Float64 - float64(whole)
		return time.Unix(whole, int64(frac*1e9))
	}
	return time.Now()
}

// flashbackMySQLSelftestWaitTick 等到实例时间戳跨过下一秒（binlog 精度为秒）。
func flashbackMySQLSelftestWaitTick(ctx context.Context, db *sql.DB) {
	start := flashbackMySQLServerNow(ctx, db)
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		time.Sleep(200 * time.Millisecond)
		if flashbackMySQLServerNow(ctx, db).Unix() > start.Unix() {
			return
		}
	}
	time.Sleep(flashbackMySQLSelftestTick)
}

// flashbackMySQLSelftestRunWindowDML 按时间窗造数：窗前一行 → 窗内全类型 DML → 可选 mid → 窗后一行。
func flashbackMySQLSelftestRunWindowDML(ctx context.Context, db *sql.DB, dbName, tbl string, major int, out *dto.FlashbackSelftestResult, mid func() error) (ident string, target, end time.Time, ok bool) {
	ident = flashbackMySQLIdent(dbName) + "." + flashbackMySQLIdent(tbl)
	if _, err := db.ExecContext(ctx, flashbackMySQLSelftestTableSQL(dbName, tbl, major)); err != nil {
		flashbackSelftestAdd(&out.Checks, "create_table", false, err.Error())
		return ident, target, end, false
	}
	flashbackSelftestAdd(&out.Checks, "create_table", true, dbName+"."+tbl)
	flashbackMySQLSelftestAssertColCount(ctx, db, dbName, tbl, out)
	flashbackMySQLSelftestAssertCharsets(ctx, db, dbName, tbl, major, out)

	if _, err := db.ExecContext(ctx, flashbackMySQLSelftestOutsideInsert(ident, 99, flashbackMySQLSelftestBeforeMark)); err != nil {
		flashbackSelftestAdd(&out.Checks, "dml_before_window", false, err.Error())
		return ident, target, end, false
	}
	flashbackSelftestAdd(&out.Checks, "dml_before_window", true, "id=99 "+flashbackMySQLSelftestBeforeMark)
	flashbackMySQLSelftestWaitTick(ctx, db)

	target = flashbackMySQLServerNow(ctx, db)
	if _, err := db.ExecContext(ctx, flashbackMySQLSelftestInsertSQL(dbName, tbl, major)); err != nil {
		flashbackSelftestAdd(&out.Checks, "insert", false, err.Error())
		return ident, target, end, false
	}
	flashbackSelftestAdd(&out.Checks, "insert", true, "2 rows")
	if _, err := db.ExecContext(ctx, fmt.Sprintf("UPDATE %s SET c_text='world', c_dec=99.00 WHERE id=1", ident)); err != nil {
		flashbackSelftestAdd(&out.Checks, "update", false, err.Error())
		return ident, target, end, false
	}
	if _, err := db.ExecContext(ctx, fmt.Sprintf("DELETE FROM %s WHERE id=2", ident)); err != nil {
		flashbackSelftestAdd(&out.Checks, "delete", false, err.Error())
		return ident, target, end, false
	}
	flashbackSelftestAdd(&out.Checks, "update_delete", true, "id=1 updated, id=2 deleted")
	if mid != nil {
		if err := mid(); err != nil {
			flashbackSelftestAdd(&out.Checks, "dml_mid", false, err.Error())
			return ident, target, end, false
		}
	}
	flashbackMySQLSelftestWaitTick(ctx, db)

	end = flashbackMySQLServerNow(ctx, db)
	if !end.After(target) {
		flashbackMySQLSelftestWaitTick(ctx, db)
		end = flashbackMySQLServerNow(ctx, db)
	}
	flashbackMySQLSelftestWaitTick(ctx, db)
	if _, err := db.ExecContext(ctx, flashbackMySQLSelftestOutsideInsert(ident, 98, flashbackMySQLSelftestAfterMark)); err != nil {
		flashbackSelftestAdd(&out.Checks, "dml_after_window", false, err.Error())
		return ident, target, end, false
	}
	flashbackSelftestAdd(&out.Checks, "dml_after_window", true, "id=98 "+flashbackMySQLSelftestAfterMark)
	flashbackSelftestAdd(&out.Checks, "time_window", true,
		fmt.Sprintf("%s ~ %s", target.Format(time.RFC3339Nano), end.Format(time.RFC3339Nano)))
	return ident, target, end, true
}

func flashbackMySQLSelftestAssertTimeWindow(out *dto.FlashbackSelftestResult, undo, redo []string) {
	all := append(append([]string{}, undo...), redo...)
	flashbackSelftestAdd(&out.Checks, "time_window_include",
		flashbackUndoHas(undo, "'hello'") && flashbackUndoHas(redo, "'world'"),
		"窗内 DML 已进入闪回/解析 SQL")
	flashbackSelftestAdd(&out.Checks, "time_window_exclude_before",
		!flashbackUndoHas(all, flashbackMySQLSelftestBeforeMark),
		"窗前 DML 未进入闪回/解析 SQL")
	flashbackSelftestAdd(&out.Checks, "time_window_exclude_after",
		!flashbackUndoHas(all, flashbackMySQLSelftestAfterMark),
		"窗后 DML 未进入闪回/解析 SQL")
}

func flashbackMySQLSelftestAddPrecheckWindow(out *dto.FlashbackSelftestResult, pre *dto.FlashbackPrecheckResult) {
	if pre == nil {
		flashbackSelftestAdd(&out.Checks, "parse_mode", false, "precheck 为空")
		flashbackSelftestAdd(&out.Checks, "precheck_time_window", false, "precheck 为空")
		return
	}
	flashbackSelftestAdd(&out.Checks, "parse_mode", pre.ParseMode == flashbackParseModeOnline,
		fmt.Sprintf("parse_mode=%s", pre.ParseMode))
	for _, it := range pre.Items {
		if it.Code == "time_window" {
			flashbackSelftestAdd(&out.Checks, "precheck_time_window", it.Status != flashbackCheckFailed, it.Message)
			return
		}
	}
	flashbackSelftestAdd(&out.Checks, "precheck_time_window", false, "预检查缺少 time_window 项")
}

func (s *FlashbackImpl) selftestMySQL(c *gin.Context, req *dto.FlashbackSelftestReq, db *sql.DB, out *dto.FlashbackSelftestResult) (*dto.FlashbackSelftestResult, error) {
	ctx := c.Request.Context()
	dbName := strings.TrimSpace(req.Database)
	major := flashbackMySQLDetectMajor(ctx, db)
	stamp := time.Now().UnixNano() % 1000000000
	tblA := fmt.Sprintf("tbl_fb_selftest_%d", stamp)
	tblB := fmt.Sprintf("tbl_fb_selftestb_%d", stamp)
	out.Table = dbName + "." + tblA
	out.Tables = []string{dbName + "." + tblA, dbName + "." + tblB}
	identA := flashbackMySQLIdent(dbName) + "." + flashbackMySQLIdent(tblA)
	identB := flashbackMySQLIdent(dbName) + "." + flashbackMySQLIdent(tblB)
	defer func() {
		_, _ = db.ExecContext(context.Background(), "DROP TABLE IF EXISTS "+identA)
		_, _ = db.ExecContext(context.Background(), "DROP TABLE IF EXISTS "+identB)
	}()

	if _, err := db.ExecContext(ctx, "CREATE DATABASE IF NOT EXISTS "+flashbackMySQLIdent(dbName)); err != nil {
		flashbackSelftestAdd(&out.Checks, "create_database", false, err.Error())
		out.OK = false
		return out, nil
	}
	_, target, end, ok := flashbackMySQLSelftestRunWindowDML(ctx, db, dbName, tblA, major, out, func() error {
		if _, err := db.ExecContext(ctx, flashbackMySQLSelftestTableSQL(dbName, tblB, major)); err != nil {
			return fmt.Errorf("create table b: %w", err)
		}
		flashbackSelftestAdd(&out.Checks, "create_table_b", true, dbName+"."+tblB)
		flashbackMySQLSelftestAssertColCount(ctx, db, dbName, tblB, out)
		if _, err := db.ExecContext(ctx, flashbackMySQLSelftestInsertSQL(dbName, tblB, major)); err != nil {
			return fmt.Errorf("insert b: %w", err)
		}
		if _, err := db.ExecContext(ctx, fmt.Sprintf("UPDATE %s SET c_text='scope_b_world', c_dec=88.00 WHERE id=1", identB)); err != nil {
			return fmt.Errorf("update b: %w", err)
		}
		if _, err := db.ExecContext(ctx, fmt.Sprintf("DELETE FROM %s WHERE id=2", identB)); err != nil {
			return fmt.Errorf("delete b: %w", err)
		}
		flashbackSelftestAdd(&out.Checks, "dml_b", true, "table b insert/update/delete")
		return nil
	})
	if !ok {
		out.OK = false
		return out, nil
	}

	base := dto.FlashbackTaskReq{
		InstanceID: strings.TrimSpace(req.InstanceID),
		Database:   dbName,
		TargetTime: target.Format(time.RFC3339Nano),
		EndTime:    end.Format(time.RFC3339Nano),
		SQLType:    "insert,update,delete",
	}
	if flashbackSelftestOutputKind(req.OutputKind) == flashback.OutputOriginal {
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
		flashbackMySQLSelftestAssertSQL(out, undoSingle, redoSingle, major)
		flashbackMySQLSelftestAssertTimeWindow(out, undoSingle, redoSingle)
		s.flashbackSelftestSubmitTicket(c, idSingle, flashbackSelftestReviewer(req), out)
	}
	flashbackSelftestAssertTableScope(out, tblA, tblB, undoSingle, undoMulti, undoAll)
	out.OK = ok1 && ok2 && ok3 && flashbackSelftestOK(out.Checks)
	return out, nil
}

func flashbackMySQLSelftestAssertColCount(ctx context.Context, db *sql.DB, schema, table string, out *dto.FlashbackSelftestResult) {
	if db == nil || out == nil {
		return
	}
	var n int
	err := db.QueryRowContext(ctx, `
SELECT COUNT(*) FROM information_schema.COLUMNS
WHERE TABLE_SCHEMA = ? AND TABLE_NAME = ?`, schema, table).Scan(&n)
	if err != nil {
		flashbackSelftestAdd(&out.Checks, "col_count", false, err.Error())
		return
	}
	flashbackSelftestAdd(&out.Checks, "col_count", n >= flashbackMySQLSelftestMinCols,
		fmt.Sprintf("%d 列（要求 ≥ %d，覆盖官网数值/字符集 utf8·utf8mb4·latin1·gbk·ascii·binary/国家字符/四档 TEXT·BLOB/日期时间/JSON/ENUM/SET/空间类型）", n, flashbackMySQLSelftestMinCols))
}

// flashbackMySQLSelftestAssertCharsets 核对列级 CHARACTER SET（官网字符集，不是只靠表默认 utf8mb4）。
func flashbackMySQLSelftestAssertCharsets(ctx context.Context, db *sql.DB, schema, table string, major int, out *dto.FlashbackSelftestResult) {
	if db == nil || out == nil {
		return
	}
	want := map[string][]string{
		"c_cs_utf8mb4":   {"utf8mb4"},
		"c_cs_utf8":      {"utf8", "utf8mb3"},
		"c_cs_latin1":    {"latin1"},
		"c_cs_gbk":       {"gbk"},
		"c_cs_ascii":     {"ascii"},
		"c_cs_binary":    {"binary", ""}, // 5.7 information_schema 对 CHARACTER SET binary 常记空
		"c_text_utf8mb4": {"utf8mb4"},
		"c_text_utf8":    {"utf8", "utf8mb3"},
		"c_text_gbk":     {"gbk"},
		"c_varchar":      {"utf8mb4"},
	}
	if major >= 8 {
		want["c_cs_utf8mb3"] = []string{"utf8mb3", "utf8"}
	}
	rows, err := db.QueryContext(ctx, `
SELECT COLUMN_NAME, CHARACTER_SET_NAME
FROM information_schema.COLUMNS
WHERE TABLE_SCHEMA = ? AND TABLE_NAME = ? AND COLUMN_NAME LIKE 'c_%'`, schema, table)
	if err != nil {
		flashbackSelftestAdd(&out.Checks, "charset_meta", false, err.Error())
		return
	}
	defer rows.Close()
	got := map[string]string{}
	for rows.Next() {
		var col, cs sql.NullString
		if err := rows.Scan(&col, &cs); err != nil {
			flashbackSelftestAdd(&out.Checks, "charset_meta", false, err.Error())
			return
		}
		if col.Valid {
			got[col.String] = strings.ToLower(strings.TrimSpace(cs.String))
		}
	}
	var miss []string
	for col, allow := range want {
		cs := got[col]
		ok := false
		for _, a := range allow {
			if cs == a {
				ok = true
				break
			}
		}
		if !ok {
			miss = append(miss, col+"="+cs)
		}
	}
	flashbackSelftestAdd(&out.Checks, "charset_utf8mb4", got["c_cs_utf8mb4"] == "utf8mb4" && got["c_text_utf8mb4"] == "utf8mb4",
		"utf8mb4 列="+got["c_cs_utf8mb4"]+" / TEXT="+got["c_text_utf8mb4"])
	flashbackSelftestAdd(&out.Checks, "charset_utf8", got["c_cs_utf8"] == "utf8" || got["c_cs_utf8"] == "utf8mb3",
		"utf8 列实际字符集="+got["c_cs_utf8"]+"（8.0+ 可能记为 utf8mb3）")
	flashbackSelftestAdd(&out.Checks, "charset_latin1", got["c_cs_latin1"] == "latin1", "latin1="+got["c_cs_latin1"])
	flashbackSelftestAdd(&out.Checks, "charset_gbk", got["c_cs_gbk"] == "gbk", "gbk="+got["c_cs_gbk"])
	flashbackSelftestAdd(&out.Checks, "charset_ascii", got["c_cs_ascii"] == "ascii", "ascii="+got["c_cs_ascii"])
	flashbackSelftestAdd(&out.Checks, "charset_binary", got["c_cs_binary"] == "binary" || got["c_cs_binary"] == "",
		"binary 列字符集="+got["c_cs_binary"]+"（5.7 可能为空）")
	flashbackSelftestAdd(&out.Checks, "charset_table_default", got["c_varchar"] == "utf8mb4",
		"表默认 utf8mb4，c_varchar="+got["c_varchar"])
	if major >= 8 {
		flashbackSelftestAdd(&out.Checks, "charset_utf8mb3", got["c_cs_utf8mb3"] == "utf8mb3" || got["c_cs_utf8mb3"] == "utf8",
			"utf8mb3="+got["c_cs_utf8mb3"])
	}
	flashbackSelftestAdd(&out.Checks, "charset_meta", len(miss) == 0,
		fmt.Sprintf("核对 %d 列字符集", len(want)))
}

func flashbackMySQLSelftestAssertSQL(out *dto.FlashbackSelftestResult, stmts, redo []string, major int) {
	upd := flashbackStmtsByPrefix(stmts, "UPDATE")
	insStmts := flashbackStmtsByPrefix(stmts, "INSERT")
	delUndo := flashbackStmtsByPrefix(stmts, "DELETE")
	flashbackSelftestAdd(&out.Checks, "flashback_update", len(upd) > 0, fmt.Sprintf("%d update", len(upd)))
	flashbackSelftestAdd(&out.Checks, "flashback_delete_as_insert", len(insStmts) > 0, fmt.Sprintf("%d insert", len(insStmts)))
	flashbackSelftestAdd(&out.Checks, "flashback_insert_as_delete", len(delUndo) > 0, fmt.Sprintf("%d delete", len(delUndo)))
	flashbackSelftestAdd(&out.Checks, "flashback_type_text", flashbackUndoHas(upd, "'hello'"), "闪回 UPDATE 还原 c_text")
	flashbackSelftestAdd(&out.Checks, "flashback_type_decimal", flashbackUndoHas(upd, "123.45"), "闪回 UPDATE 还原 c_dec")
	flashbackSelftestAdd(&out.Checks, "flashback_type_int", flashbackUndoHas(insStmts, "42"), "闪回 int")
	flashbackSelftestAdd(&out.Checks, "flashback_type_int_u", flashbackUndoHas(insStmts, "3000000001") || flashbackUndoHas(insStmts, "`c_int_u`"), "闪回 int unsigned")
	flashbackSelftestAdd(&out.Checks, "flashback_type_tiny_u", flashbackUndoHas(insStmts, "201") || flashbackUndoHas(insStmts, "`c_tiny_u`"), "闪回 tinyint unsigned")
	flashbackSelftestAdd(&out.Checks, "flashback_type_medium", flashbackUndoHas(insStmts, "100002") || flashbackUndoHas(insStmts, "`c_medium`"), "闪回 mediumint")
	flashbackSelftestAdd(&out.Checks, "flashback_type_numeric", flashbackUndoHas(insStmts, "8.5") || flashbackUndoHas(insStmts, "`c_numeric`"), "闪回 numeric")
	flashbackSelftestAdd(&out.Checks, "flashback_type_varchar", flashbackUndoHas(insStmts, "'v2'"), "闪回 varchar")
	flashbackSelftestAdd(&out.Checks, "flashback_type_tinytext", flashbackUndoHas(insStmts, "'tt2'") || flashbackUndoHas(insStmts, "`c_tinytext`"), "闪回 tinytext")
	flashbackSelftestAdd(&out.Checks, "flashback_type_mediumtext", flashbackUndoHas(insStmts, "'mt2'") || flashbackUndoHas(insStmts, "`c_mediumtext`"), "闪回 mediumtext")
	flashbackSelftestAdd(&out.Checks, "flashback_type_longtext", flashbackUndoHas(insStmts, "'lt2'") || flashbackUndoHas(insStmts, "`c_longtext`"), "闪回 longtext")
	flashbackSelftestAdd(&out.Checks, "flashback_type_json", flashbackUndoHas(insStmts, `"k":2`) || flashbackUndoHas(insStmts, `"k": 2`), "闪回 json")
	flashbackSelftestAdd(&out.Checks, "flashback_type_date", flashbackUndoHas(insStmts, "2025-12-31"), "闪回 date")
	flashbackSelftestAdd(&out.Checks, "flashback_type_time", flashbackUndoHas(insStmts, "08:30"), "闪回 time")
	flashbackSelftestAdd(&out.Checks, "flashback_type_datetime", flashbackUndoHas(insStmts, "2025-03-01"), "闪回 datetime")
	flashbackSelftestAdd(&out.Checks, "flashback_type_year", flashbackUndoHas(insStmts, "2025") || flashbackUndoHas(insStmts, "`c_year`"), "闪回 year")
	flashbackSelftestAdd(&out.Checks, "flashback_type_blob", flashbackUndoHas(insStmts, "beef") || flashbackUndoHas(insStmts, "BEEF"), "闪回 blob/binary")
	flashbackSelftestAdd(&out.Checks, "flashback_type_enum", flashbackUndoHas(insStmts, "'b'") || flashbackUndoHas(insStmts, "`c_enum`"), "闪回 enum")
	flashbackSelftestAdd(&out.Checks, "flashback_type_set", flashbackUndoHas(insStmts, "'y'") || flashbackUndoHas(insStmts, "`c_set`"), "闪回 set")
	flashbackSelftestAdd(&out.Checks, "flashback_type_bit", flashbackUndoHas(insStmts, "`c_bit`") || flashbackUndoHas(insStmts, "204"), "闪回 bit")
	flashbackSelftestAdd(&out.Checks, "flashback_type_geom",
		flashbackUndoHas(insStmts, "`c_point`") && flashbackUndoHas(insStmts, "`c_polygon`") && flashbackUndoHas(insStmts, "`c_geomcoll`"),
		"闪回 空间类型列")
	flashbackSelftestAdd(&out.Checks, "flashback_type_integer", flashbackUndoHas(insStmts, "`c_integer`") || flashbackUndoHas(insStmts, "42"), "闪回 INTEGER 同义名")
	flashbackSelftestAdd(&out.Checks, "flashback_type_nchar", flashbackUndoHas(insStmts, "'nch2'") || flashbackUndoHas(insStmts, "`c_nchar`"), "闪回 NCHAR")
	flashbackSelftestAdd(&out.Checks, "flashback_type_time6", flashbackUndoHas(insStmts, "08:30:00") || flashbackUndoHas(insStmts, "`c_time6`"), "闪回 TIME(6)")
	flashbackSelftestAdd(&out.Checks, "flashback_type_enum2", flashbackUndoHas(insStmts, "large") || flashbackUndoHas(insStmts, "`c_enum2`"), "闪回 ENUM 第二组")
	flashbackSelftestAdd(&out.Checks, "flashback_type_set2", flashbackUndoHas(insStmts, "blue") || flashbackUndoHas(insStmts, "`c_set2`"), "闪回 SET 第二组")
	flashbackSelftestAdd(&out.Checks, "flashback_type_json2", flashbackUndoHas(insStmts, `"x":2`) || flashbackUndoHas(insStmts, `"x": 2`) || flashbackUndoHas(insStmts, "`c_json2`"), "闪回 JSON 第二组")
	flashbackSelftestAdd(&out.Checks, "flashback_type_cs_utf8mb4",
		flashbackUndoHas(insStmts, "世界mb4") || flashbackUndoHas(insStmts, "`c_cs_utf8mb4`"),
		"闪回 utf8mb4（含中文/补充平面）")
	flashbackSelftestAdd(&out.Checks, "flashback_type_cs_utf8",
		flashbackUndoHas(insStmts, "世界utf8") || flashbackUndoHas(insStmts, "`c_cs_utf8`"),
		"闪回 utf8/utf8mb3 中文")
	flashbackSelftestAdd(&out.Checks, "flashback_type_cs_gbk",
		flashbackUndoHas(insStmts, "汉字GBK2") || flashbackUndoHas(insStmts, "汉字gbk") || flashbackUndoHas(insStmts, "`c_cs_gbk`"),
		"闪回 gbk 中文")
	flashbackSelftestAdd(&out.Checks, "flashback_type_cs_latin1",
		flashbackUndoHas(insStmts, "naiv") || flashbackUndoHas(insStmts, "`c_cs_latin1`"),
		"闪回 latin1")
	flashbackSelftestAdd(&out.Checks, "flashback_type_cs_ascii",
		flashbackUndoHas(insStmts, "ascii2") || flashbackUndoHas(insStmts, "`c_cs_ascii`"),
		"闪回 ascii")
	flashbackSelftestAdd(&out.Checks, "flashback_type_text_utf8mb4",
		flashbackUndoHas(insStmts, "世mb4") || flashbackUndoHas(insStmts, "`c_text_utf8mb4`"),
		"闪回 TEXT utf8mb4")
	if flashbackMySQLHasVector(major) {
		flashbackSelftestAdd(&out.Checks, "flashback_type_vector",
			flashbackUndoHas(insStmts, "`c_vector`") || flashbackUndoHas(insStmts, "4") && flashbackUndoHas(insStmts, "5"),
			"闪回 VECTOR(3)")
	}

	redoUpd := flashbackStmtsByPrefix(redo, "UPDATE")
	redoIns := flashbackStmtsByPrefix(redo, "INSERT")
	redoDel := flashbackStmtsByPrefix(redo, "DELETE")
	flashbackSelftestAdd(&out.Checks, "parse_insert", len(redoIns) > 0, fmt.Sprintf("%d insert", len(redoIns)))
	flashbackSelftestAdd(&out.Checks, "parse_update", len(redoUpd) > 0, fmt.Sprintf("%d update", len(redoUpd)))
	flashbackSelftestAdd(&out.Checks, "parse_delete", len(redoDel) > 0, fmt.Sprintf("%d delete", len(redoDel)))
	flashbackSelftestAdd(&out.Checks, "parse_type_text", flashbackUndoHas(redoUpd, "'world'"), "解析 UPDATE 新 c_text")
	flashbackSelftestAdd(&out.Checks, "parse_type_decimal", flashbackUndoHas(redoUpd, "99"), "解析 UPDATE 新 c_dec")
	flashbackSelftestAdd(&out.Checks, "parse_delete_id", flashbackUndoHas(redoDel, "2"), "解析 DELETE id=2")
}

// flashbackVerifyDirectMySQL 直连 MySQL 做全类型 DML + BINLOG DUMP 断言，不依赖 instance_id / Hub 任务表。
func flashbackVerifyDirectMySQL(ctx context.Context, db *sql.DB, creds flashbackMySQLCreds) *dto.FlashbackSelftestResult {
	out := &dto.FlashbackSelftestResult{Checks: []dto.FlashbackSelftestCheck{}}
	ver := flashbackMySQLVar(ctx, db, "version")
	major, minor := flashbackMySQLParseVersion(ver)
	verOK := major > 5 || (major == 5 && minor >= 7)
	flashbackSelftestAdd(&out.Checks, "version", verOK, ver)
	if !verOK {
		out.OK = false
		return out
	}

	format := flashbackMySQLVar(ctx, db, "binlog_format")
	image := flashbackMySQLVar(ctx, db, "binlog_row_image")
	if st, msg := flashbackMySQLFormatGate(format, image); st == flashbackCheckFailed {
		flashbackSelftestAdd(&out.Checks, "binlog_format", false, msg)
		out.OK = false
		return out
	}
	flashbackSelftestAdd(&out.Checks, "binlog_format", true, format+"/"+image)

	dbName := strings.TrimSpace(creds.DBName)
	if dbName == "" {
		dbName = "fbtest"
	}
	tbl := fmt.Sprintf("tbl_fb_selftest_%d", time.Now().UnixNano()%1000000000)
	out.Table = dbName + "." + tbl
	ident := flashbackMySQLIdent(dbName) + "." + flashbackMySQLIdent(tbl)
	defer func() { _, _ = db.ExecContext(context.Background(), "DROP TABLE IF EXISTS "+ident) }()

	if _, err := db.ExecContext(ctx, "CREATE DATABASE IF NOT EXISTS "+flashbackMySQLIdent(dbName)); err != nil {
		flashbackSelftestAdd(&out.Checks, "create_database", false, err.Error())
		out.OK = false
		return out
	}
	_, target, end, ok := flashbackMySQLSelftestRunWindowDML(ctx, db, dbName, tbl, major, out, nil)
	if !ok {
		out.OK = false
		return out
	}

	logs, err := flashbackMySQLListBinlogs(ctx, db)
	if err != nil || len(logs) == 0 {
		msg := "实例无可用 binlog"
		if err != nil {
			msg = err.Error()
		}
		flashbackSelftestAdd(&out.Checks, "binary_logs", false, msg)
		out.OK = false
		return out
	}
	startIdx := flashbackMySQLPickStartFile(ctx, creds, logs, target)
	if startIdx < 0 || startIdx >= len(logs) {
		startIdx = 0
	}
	startFile := logs[startIdx].Name
	flashbackSelftestAdd(&out.Checks, "pick_start_file", true,
		fmt.Sprintf("%s (idx=%d/%d)", startFile, startIdx, len(logs)))

	endFile, endPos, err := flashbackMySQLMasterStatus(ctx, db)
	if err != nil {
		flashbackSelftestAdd(&out.Checks, "master_status_end", false, err.Error())
		out.OK = false
		return out
	}
	flashbackSelftestAdd(&out.Checks, "master_status_end", true, fmt.Sprintf("%s:%d", endFile, endPos))

	dict, err := flashbackLoadMySQLDictionary(ctx, db, dbName, []string{out.Table})
	if err != nil {
		flashbackSelftestAdd(&out.Checks, "dictionary", false, err.Error())
		out.OK = false
		return out
	}
	flashbackSelftestAdd(&out.Checks, "dictionary", true, fmt.Sprintf("%d tables", len(dict.Wanted)))

	dumpID := flashbackMySQLDumpServerID("selftest-direct:" + tbl)
	var undo, redo []string
	st, err := flashbackDumpMySQLBinlog(ctx, creds, dumpID, flashbackMySQLDumpOpt{
		StartFile: startFile, StartPos: 4,
		EndFile: endFile, EndPos: endPos,
		Target: target, End: end,
		MaxBytes: flashbackDefaultMaxWALBytes,
	}, dict, nil, func(ch flashbackChange) bool {
		if flashbackIsDDLOp(ch.Op) {
			return true
		}
		if dict.match(ch.Schema, ch.Table) == nil {
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
		flashbackSelftestAdd(&out.Checks, "parse", false, err.Error())
		out.OK = false
		return out
	}
	flashbackSelftestAdd(&out.Checks, "parse", len(undo) > 0, st.String())
	out.UndoSQL = undo
	out.ParseSQL = redo
	if len(undo) == 0 {
		flashbackSelftestAdd(&out.Checks, "flashback_sql", false, "未生成闪回 SQL")
		out.OK = false
		return out
	}
	flashbackSelftestAdd(&out.Checks, "flashback_sql", true, fmt.Sprintf("%d undo", len(undo)))
	if len(redo) == 0 {
		flashbackSelftestAdd(&out.Checks, "parse_sql", false, "未生成解析 SQL")
		out.OK = false
		return out
	}
	flashbackSelftestAdd(&out.Checks, "parse_sql", true, fmt.Sprintf("%d redo", len(redo)))
	flashbackMySQLSelftestAssertSQL(out, undo, redo, major)
	flashbackMySQLSelftestAssertTimeWindow(out, undo, redo)
	out.OK = flashbackSelftestOK(out.Checks)
	return out
}

func flashbackMySQLCollectChanges(ctx context.Context, creds flashbackMySQLCreds, opt flashbackMySQLDumpOpt, dict *flashbackMySQLDict) ([]flashbackChange, flashbackMySQLDumpStat, error) {
	var changes []flashbackChange
	st, err := flashbackDumpMySQLBinlog(ctx, creds, flashbackMySQLDumpServerID("selftest-deep:"+opt.StartFile), opt, dict, nil, func(ch flashbackChange) bool {
		changes = append(changes, ch)
		return true
	})
	return changes, st, err
}

func flashbackMySQLChangesSQL(changes []flashbackChange, tables []string, op string) (undo, redo []string) {
	want := map[string]struct{}{}
	for _, t := range tables {
		want[strings.ToLower(strings.TrimSpace(t))] = struct{}{}
	}
	op = strings.ToUpper(strings.TrimSpace(op))
	for _, ch := range changes {
		if op != "" && !strings.EqualFold(ch.Op, op) {
			continue
		}
		if len(want) > 0 {
			key := strings.ToLower(ch.Schema + "." + ch.Table)
			if _, ok := want[key]; !ok {
				if _, ok := want[strings.ToLower(ch.Table)]; !ok {
					continue
				}
			}
		}
		if flashbackIsDDLOp(ch.Op) && op != "" && !flashbackIsDDLOp(op) {
			continue
		}
		if u, _ := flashbackUndoSQL(ch); u != "" {
			undo = append(undo, u)
		}
		if r, _ := flashbackRedoSQL(ch); r != "" {
			redo = append(redo, r)
		}
	}
	return undo, redo
}

func flashbackMySQLAssertOriginalByType(out *dto.FlashbackSelftestResult, dbName, tblA, tblB string, changes []flashbackChange) {
	qualA := dbName + "." + tblA
	qualB := dbName + "." + tblB
	scopes := []struct {
		prefix string
		tables []string
		scope  string
		want   map[string]int
	}{
		{"orig_single", []string{qualA}, "single", map[string]int{"insert": 2, "update": 1, "delete": 1}},
		{"orig_multi", []string{qualA, qualB}, "multi", map[string]int{"insert": 4, "update": 2, "delete": 2}},
		{"orig_all", nil, "all", map[string]int{"insert": 4, "update": 2, "delete": 2}},
	}
	verbs := map[string]string{"insert": "INSERT", "update": "UPDATE", "delete": "DELETE"}
	for _, sqlType := range []string{"insert", "update", "delete"} {
		for _, sc := range scopes {
			_, redo := flashbackMySQLChangesSQL(changes, sc.tables, verbs[sqlType])
			prefix := sc.prefix + "_" + sqlType
			flashbackSelftestAssertOriginalSQL(out, prefix, verbs[sqlType], tblA, tblB, sc.scope, redo, sc.want[sqlType])
		}
	}
}

// flashbackVerifyDeepMySQL 双表 + 闪回/原始 SQL + 字典快照 + file:pos + XID + DDL 审计。
func flashbackVerifyDeepMySQL(ctx context.Context, db *sql.DB, creds flashbackMySQLCreds) *dto.FlashbackSelftestResult {
	out := &dto.FlashbackSelftestResult{Checks: []dto.FlashbackSelftestCheck{}}
	ver := flashbackMySQLVar(ctx, db, "version")
	major, minor := flashbackMySQLParseVersion(ver)
	verOK := major > 5 || (major == 5 && minor >= 7)
	flashbackSelftestAdd(&out.Checks, "version", verOK, ver)
	if !verOK {
		out.OK = false
		return out
	}
	format := flashbackMySQLVar(ctx, db, "binlog_format")
	image := flashbackMySQLVar(ctx, db, "binlog_row_image")
	if st, msg := flashbackMySQLFormatGate(format, image); st == flashbackCheckFailed {
		flashbackSelftestAdd(&out.Checks, "binlog_format", false, msg)
		out.OK = false
		return out
	}
	flashbackSelftestAdd(&out.Checks, "binlog_format", true, format+"/"+image)

	dbName := strings.TrimSpace(creds.DBName)
	if dbName == "" {
		dbName = "fbtest"
	}
	stamp := time.Now().UnixNano() % 1000000000
	tblA := fmt.Sprintf("tbl_fb_selftest_%d", stamp)
	tblB := fmt.Sprintf("tbl_fb_selftestb_%d", stamp)
	out.Table = dbName + "." + tblA
	out.Tables = []string{dbName + "." + tblA, dbName + "." + tblB}
	identA := flashbackMySQLIdent(dbName) + "." + flashbackMySQLIdent(tblA)
	identB := flashbackMySQLIdent(dbName) + "." + flashbackMySQLIdent(tblB)
	defer func() {
		_, _ = db.ExecContext(context.Background(), "DROP TABLE IF EXISTS "+identA)
		_, _ = db.ExecContext(context.Background(), "DROP TABLE IF EXISTS "+identB)
	}()
	if _, err := db.ExecContext(ctx, "CREATE DATABASE IF NOT EXISTS "+flashbackMySQLIdent(dbName)); err != nil {
		flashbackSelftestAdd(&out.Checks, "create_database", false, err.Error())
		out.OK = false
		return out
	}
	_, target, end, ok := flashbackMySQLSelftestRunWindowDML(ctx, db, dbName, tblA, major, out, func() error {
		if _, err := db.ExecContext(ctx, flashbackMySQLSelftestTableSQL(dbName, tblB, major)); err != nil {
			return fmt.Errorf("create table b: %w", err)
		}
		flashbackSelftestAdd(&out.Checks, "create_table_b", true, dbName+"."+tblB)
		flashbackMySQLSelftestAssertColCount(ctx, db, dbName, tblB, out)
		if _, err := db.ExecContext(ctx, flashbackMySQLSelftestInsertSQL(dbName, tblB, major)); err != nil {
			return fmt.Errorf("insert b: %w", err)
		}
		if _, err := db.ExecContext(ctx, fmt.Sprintf("UPDATE %s SET c_text='scope_b_world', c_dec=88.00 WHERE id=1", identB)); err != nil {
			return fmt.Errorf("update b: %w", err)
		}
		if _, err := db.ExecContext(ctx, fmt.Sprintf("DELETE FROM %s WHERE id=2", identB)); err != nil {
			return fmt.Errorf("delete b: %w", err)
		}
		flashbackSelftestAdd(&out.Checks, "dml_b", true, "table b insert/update/delete")
		return nil
	})
	if !ok {
		out.OK = false
		return out
	}

	logs, err := flashbackMySQLListBinlogs(ctx, db)
	if err != nil || len(logs) == 0 {
		msg := "实例无可用 binlog"
		if err != nil {
			msg = err.Error()
		}
		flashbackSelftestAdd(&out.Checks, "binary_logs", false, msg)
		out.OK = false
		return out
	}
	startIdx := flashbackMySQLPickStartFile(ctx, creds, logs, target)
	if startIdx < 0 || startIdx >= len(logs) {
		startIdx = 0
	}
	picked := logs[startIdx].Name
	masterFile, masterPos, err := flashbackMySQLMasterStatus(ctx, db)
	if err != nil {
		flashbackSelftestAdd(&out.Checks, "master_status_end", false, err.Error())
		out.OK = false
		return out
	}
	startFile, startPos, endFile, endPos, err := flashbackMySQLResolveDumpRange(
		picked, 4, masterFile, masterPos, logs, picked, masterFile, masterPos)
	if err != nil {
		flashbackSelftestAdd(&out.Checks, "file_pos", false, err.Error())
		out.OK = false
		return out
	}
	flashbackSelftestAdd(&out.Checks, "file_pos", startFile == picked && startPos == 4 && endFile == masterFile,
		fmt.Sprintf("DUMP %s:%d → %s:%d", startFile, startPos, endFile, endPos))

	live, err := flashbackLoadMySQLDictionary(ctx, db, dbName, out.Tables)
	if err != nil {
		flashbackSelftestAdd(&out.Checks, "dictionary", false, err.Error())
		out.OK = false
		return out
	}
	snapPath := filepath.Join(os.TempDir(), "jupiter-flashback-mysql-deep", fmt.Sprintf("%d", stamp), flashbackDictFileName)
	if err := flashbackSaveMySQLDictionaryFile(snapPath, live); err != nil {
		flashbackSelftestAdd(&out.Checks, "dict_snap", false, err.Error())
		out.OK = false
		return out
	}
	dict, err := flashbackLoadMySQLDictionaryFile(snapPath)
	if err != nil {
		flashbackSelftestAdd(&out.Checks, "dict_snap", false, err.Error())
		out.OK = false
		return out
	}
	flashbackSelftestAdd(&out.Checks, "dict_snap", len(dict.Wanted) >= 2, fmt.Sprintf("%d tables from snapshot", len(dict.Wanted)))

	changes, st, err := flashbackMySQLCollectChanges(ctx, creds, flashbackMySQLDumpOpt{
		StartFile: startFile, StartPos: startPos,
		EndFile: endFile, EndPos: endPos,
		Target: target, End: end,
		MaxBytes: flashbackDefaultMaxWALBytes,
	}, dict)
	if err != nil {
		flashbackSelftestAdd(&out.Checks, "parse", false, err.Error())
		out.OK = false
		return out
	}
	flashbackSelftestAdd(&out.Checks, "parse", st.Rows > 0, st.String())

	var xidOK, ddlN int
	for _, ch := range changes {
		if flashbackChangeXID(ch) > 0 {
			xidOK++
		}
		if flashbackIsDDLOp(ch.Op) {
			ddlN++
		}
	}
	flashbackSelftestAdd(&out.Checks, "xid_event", xidOK > 0, fmt.Sprintf("%d changes with XID", xidOK))
	flashbackSelftestAdd(&out.Checks, "ddl_audit", ddlN > 0 && st.DDLSkipped > 0,
		fmt.Sprintf("ddl_changes=%d ddl_skipped=%d", ddlN, st.DDLSkipped))
	ddlUndo, ddlRedo := flashbackMySQLChangesSQL(changes, nil, "CREATE")
	flashbackSelftestAdd(&out.Checks, "ddl_create_audit",
		len(ddlRedo) > 0 && len(ddlUndo) == 0 && flashbackUndoHas(ddlRedo, "CREATE TABLE"),
		fmt.Sprintf("%d CREATE redo, undo=%d", len(ddlRedo), len(ddlUndo)))

	undo, redo := flashbackMySQLChangesSQL(changes, []string{out.Table}, "")
	out.UndoSQL = undo
	out.ParseSQL = redo
	flashbackSelftestAdd(&out.Checks, "flashback_sql", len(undo) > 0, fmt.Sprintf("%d undo", len(undo)))
	flashbackSelftestAdd(&out.Checks, "parse_sql", len(redo) > 0, fmt.Sprintf("%d redo", len(redo)))
	if len(undo) == 0 || len(redo) == 0 {
		out.OK = false
		return out
	}
	flashbackMySQLSelftestAssertSQL(out, undo, redo, major)
	flashbackMySQLSelftestAssertTimeWindow(out, undo, redo)
	undoMulti, _ := flashbackMySQLChangesSQL(changes, out.Tables, "")
	undoAll, _ := flashbackMySQLChangesSQL(changes, nil, "")
	flashbackSelftestAssertTableScope(out, tblA, tblB, undo, undoMulti, undoAll)
	flashbackMySQLAssertOriginalByType(out, dbName, tblA, tblB, changes)
	out.OK = flashbackSelftestOK(out.Checks)
	return out
}
