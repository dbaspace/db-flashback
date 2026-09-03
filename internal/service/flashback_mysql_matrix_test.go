package service

import (
	"context"
	"database/sql"
	"os"
	"strings"
	"testing"
	"time"

	_ "github.com/go-sql-driver/mysql"
)

func TestFlashbackMySQLParseVersion(t *testing.T) {
	maj, min := flashbackMySQLParseVersion("5.7.44-log")
	if maj != 5 || min != 7 {
		t.Fatalf("5.7.44 got %d.%d", maj, min)
	}
	maj, min = flashbackMySQLParseVersion("8.0.41")
	if maj != 8 || min != 0 {
		t.Fatalf("8.0.41 got %d.%d", maj, min)
	}
	maj, min = flashbackMySQLParseVersion("8.4.4-commercial")
	if maj != 8 || min != 4 {
		t.Fatalf("8.4.4 got %d.%d", maj, min)
	}
	maj, min = flashbackMySQLParseVersion("9.7.2")
	if maj != 9 || min != 7 {
		t.Fatalf("9.7.2 got %d.%d", maj, min)
	}
	maj, min = flashbackMySQLParseVersion("26.7.0")
	if maj != 26 || min != 7 {
		t.Fatalf("26.7.0 got %d.%d", maj, min)
	}
	if !flashbackMySQLHasVector(9) || !flashbackMySQLHasVector(26) || flashbackMySQLHasVector(8) {
		t.Fatal("vector gate")
	}
}

func TestFlashbackMySQLSelftestTableCoversTypes(t *testing.T) {
	sql57 := flashbackMySQLSelftestTableSQL("fb", "t", 5)
	need := []string{
		"TINYINT", "TINYINT UNSIGNED", "SMALLINT", "SMALLINT UNSIGNED",
		"MEDIUMINT", "MEDIUMINT UNSIGNED", "INT", "INT UNSIGNED",
		"BIGINT", "BIGINT UNSIGNED", "DECIMAL", "DECIMAL(12,2) UNSIGNED", "NUMERIC",
		"FLOAT", "FLOAT UNSIGNED", "DOUBLE", "DOUBLE UNSIGNED", "REAL", "BIT", "BIT(64)",
		"CHAR", "VARCHAR", "BINARY", "VARBINARY",
		"TINYTEXT", "TEXT", "MEDIUMTEXT", "LONGTEXT",
		"TINYBLOB", "BLOB", "MEDIUMBLOB", "LONGBLOB",
		"ENUM", "SET", "DATE", "TIME", "DATETIME", "TIMESTAMP", "YEAR", "JSON",
		"GEOMETRY", "POINT", "LINESTRING", "POLYGON",
		"MULTIPOINT", "MULTILINESTRING", "MULTIPOLYGON", "GEOMETRYCOLLECTION",
		"INTEGER", "DEC(", "FIXED", "DOUBLE PRECISION", "BOOL", "BIT(1)", "BIT(32)",
		"TIME(6)", "DATETIME(6)", "TIMESTAMP(6)", "NCHAR", "NVARCHAR", "NATIONAL CHAR",
		"NATIONAL VARCHAR", "ZEROFILL",
		"CHARACTER SET utf8mb4", "CHARACTER SET utf8", "CHARACTER SET latin1",
		"CHARACTER SET gbk", "CHARACTER SET ascii", "CHARACTER SET binary",
	}
	up := strings.ToUpper(sql57)
	for _, n := range need {
		haystack := up
		if strings.Contains(n, "CHARACTER SET") {
			haystack = sql57
		}
		if !strings.Contains(haystack, n) {
			t.Fatalf("5.7 table missing type %s", n)
		}
	}
	if strings.Contains(up, "VECTOR") {
		t.Fatal("5.7 table should not contain VECTOR")
	}
	if strings.Contains(sql57, "utf8mb3") {
		t.Fatal("5.7 table should not use utf8mb3 name")
	}
	sql8 := flashbackMySQLSelftestTableSQL("fb", "t", 8)
	if !strings.Contains(sql8, "utf8mb3") {
		t.Fatal("8.0 table should contain utf8mb3")
	}
	sql9 := flashbackMySQLSelftestTableSQL("fb", "t", 9)
	if !strings.Contains(strings.ToUpper(sql9), "VECTOR") {
		t.Fatal("9.x table should contain VECTOR")
	}
	ins := flashbackMySQLSelftestInsertSQL("fb", "t", 5)
	if !strings.Contains(ins, "ST_GeomFromText") || !strings.Contains(ins, `{"k":2}`) {
		t.Fatal("insert missing spatial/json")
	}
	n := 0
	for _, line := range strings.Split(sql57, "\n") {
		s := strings.TrimSpace(line)
		if strings.HasPrefix(s, "id ") || strings.HasPrefix(s, "c_") {
			n++
		}
	}
	if n < flashbackMySQLSelftestMinCols {
		t.Fatalf("selftest table has %d cols, want >= %d", n, flashbackMySQLSelftestMinCols)
	}
}

func TestFlashbackMySQLMatrix(t *testing.T) {
	if os.Getenv("FLASHBACK_MYSQL_MATRIX") == "" {
		t.Skip("set FLASHBACK_MYSQL_MATRIX=1 用 Docker 启动 MySQL 5.7/8.0/8.4/9.7/26.7 并验证 binlog 闪回")
	}
	if !flashbackDockerAvailable() {
		t.Fatal("需要 Docker（与 PG 矩阵相同：docker run 拉起各版本）")
	}
	t.Logf("runtime=docker")
	ctx := context.Background()
	var failed []string
	for _, spec := range flashbackMySQLMatrixSelected() {
		spec := spec
		ok := t.Run("mysql"+spec.Name, func(t *testing.T) {
			creds, cleanup, err := flashbackStartMySQLMatrix(spec)
			if err != nil {
				t.Fatalf("start: %v", err)
			}
			defer cleanup()
			db, err := sql.Open("mysql", flashbackMySQLDSN(creds))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			db.SetConnMaxLifetime(2 * time.Minute)
			ctx, cancel := context.WithTimeout(ctx, 6*time.Minute)
			defer cancel()
			res := flashbackVerifyDirectMySQL(ctx, db, creds)
			for _, c := range res.Checks {
				if !c.OK {
					t.Logf("FAIL %s: %s", c.Name, c.Detail)
				}
			}
			if !res.OK {
				t.Fatalf("MySQL %s 自测失败，checks=%d undo=%d", spec.Name, len(res.Checks), len(res.UndoSQL))
			}
			t.Logf("MySQL %s PASS table=%s undo=%d redo=%d", spec.Name, res.Table, len(res.UndoSQL), len(res.ParseSQL))
		})
		if !ok {
			failed = append(failed, spec.Name)
		}
	}
	if len(failed) > 0 {
		t.Fatalf("failed versions: %s", strings.Join(failed, ","))
	}
}
