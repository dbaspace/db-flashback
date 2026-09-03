package service

import (
	"context"
	"database/sql"
	"os"
	"testing"
	"time"

	_ "github.com/go-sql-driver/mysql"
	_ "github.com/lib/pq"

	"db-flashback/internal/service/dto"
)

// TestFlashbackPostgresDeepSelftest 启动本地 PG16（或 FLASHBACK_PG_DSN）跑深度闪回断言。
func TestFlashbackPostgresDeepSelftest(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()

	if dsn := os.Getenv("FLASHBACK_PG_DSN"); dsn != "" {
		db, err := sql.Open("postgres", dsn)
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		res := flashbackVerifyDirect(ctx, db)
		flashbackLogSelftest(t, res)
		if !res.OK {
			t.Fatalf("DSN 深度自测失败 checks=%d", len(res.Checks))
		}
		return
	}

	rt, err := flashbackStartPGMatrix(16)
	if err != nil {
		t.Skipf("无法启动本地 PG16: %v", err)
	}
	defer rt.Close()
	db, err := sql.Open("postgres", rt.DSN)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	res := flashbackVerifyDirect(ctx, db)
	flashbackLogSelftest(t, res)
	if !res.OK {
		t.Fatalf("PG16 深度自测失败 kind=%s checks=%d", rt.Kind, len(res.Checks))
	}
}

// TestFlashbackMySQLDeepSelftest 启动本地 MySQL 8.0（或 FLASHBACK_MYSQL_* 环境变量）跑闪回 + 原始 SQL 深度断言。
func TestFlashbackMySQLDeepSelftest(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()

	if creds, ok := flashbackMySQLCredsFromEnv("", 0, "fbtest"); ok && creds.Host != "" {
		db, err := sql.Open("mysql", flashbackMySQLDSN(creds))
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		res := flashbackVerifyDeepMySQL(ctx, db, creds)
		flashbackLogSelftest(t, res)
		if !res.OK {
			t.Fatalf("DSN MySQL 深度自测失败 checks=%d", len(res.Checks))
		}
		return
	}

	spec := flashbackMySQLMatrixSpec{Name: "deep8", Image: "mysql:8.0", Port: 13408}
	creds, cleanup, err := flashbackStartMySQLMatrix(spec)
	if err != nil {
		t.Skipf("无法启动本地 MySQL 8.0: %v", err)
	}
	defer cleanup()
	db, err := sql.Open("mysql", flashbackMySQLDSN(creds))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetConnMaxLifetime(2 * time.Minute)
	res := flashbackVerifyDeepMySQL(ctx, db, creds)
	flashbackLogSelftest(t, res)
	if !res.OK {
		t.Fatalf("MySQL 8.0 深度自测失败 checks=%d undo=%d redo=%d", len(res.Checks), len(res.UndoSQL), len(res.ParseSQL))
	}
}

func flashbackLogSelftest(t *testing.T, res *dto.FlashbackSelftestResult) {
	t.Helper()
	if res == nil {
		t.Fatal("nil result")
	}
	fail := 0
	for _, c := range res.Checks {
		if !c.OK {
			fail++
			t.Logf("FAIL %s: %s", c.Name, c.Detail)
		} else {
			t.Logf("OK   %s: %s", c.Name, c.Detail)
		}
	}
	t.Logf("ok=%v fail=%d/%d undo=%d redo=%d", res.OK, fail, len(res.Checks), len(res.UndoSQL), len(res.ParseSQL))
}
