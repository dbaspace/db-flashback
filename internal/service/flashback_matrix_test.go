package service

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "github.com/lib/pq"
)

func TestFlashbackPostgresMatrix(t *testing.T) {
	if os.Getenv("FLASHBACK_PG_MATRIX") == "" {
		t.Skip("set FLASHBACK_PG_MATRIX=1 启动 PG 并验证单表/多表/整库闪回任务")
	}
	ctx := context.Background()
	kind := "binary"
	if flashbackDockerAvailable() {
		kind = "docker"
	}
	t.Logf("runtime=%s", kind)

	var failed []string
	for _, spec := range flashbackMatrixSelected() {
		spec := spec
		ok := t.Run(fmt.Sprintf("pg%d", spec.Major), func(t *testing.T) {
			rt, err := flashbackStartPGMatrix(spec.Major)
			if err != nil {
				t.Fatalf("start: %v", err)
			}
			defer rt.Close()
			db, err := sql.Open("postgres", rt.DSN)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			db.SetConnMaxLifetime(2 * time.Minute)
			res := flashbackVerifyDirect(ctx, db)
			for _, c := range res.Checks {
				if !c.OK || strings.HasPrefix(c.Name, "opt_parse") || strings.HasPrefix(c.Name, "opt_truncate") {
					t.Logf("%s %s: %s", map[bool]string{true: "OK", false: "FAIL"}[c.OK], c.Name, c.Detail)
				}
			}
			if !res.OK {
				t.Fatalf("PG%d (%s) 自测失败，checks=%d", spec.Major, rt.Kind, len(res.Checks))
			}
			t.Logf("PG%d %s PASS table=%s undo=%d redo=%d", spec.Major, rt.Kind, res.Table, len(res.UndoSQL), len(res.ParseSQL))
		})
		if !ok {
			failed = append(failed, fmt.Sprintf("pg%d", spec.Major))
		}
	}
	_ = os.RemoveAll(filepath.Join(os.TempDir(), "hub-fb-pg-matrix"))
	_ = os.RemoveAll(filepath.Join(os.TempDir(), "jupiter-flashback-matrix"))
	if len(failed) > 0 {
		t.Fatalf("failed versions: %s", strings.Join(failed, ","))
	}
}
