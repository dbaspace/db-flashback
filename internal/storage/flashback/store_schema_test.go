package flashback

import (
	"strings"
	"testing"
)

func TestCollectMissingFlashbackTables_allPresent(t *testing.T) {
	found := map[string]bool{
		"tbl_flashback_tasks":     true,
		"tbl_flashback_logs":      true,
		"tbl_flashback_sqls":      true,
		"tbl_flashback_args":      true,
		"tbl_flashback_instances": true,
		"tbl_flashback_artifacts": true,
	}
	if got := collectMissingFlashbackTables(found); len(got) != 0 {
		t.Fatalf("missing=%v want none", got)
	}
}

func TestCollectMissingFlashbackTables_partial(t *testing.T) {
	found := map[string]bool{"tbl_flashback_tasks": true}
	got := collectMissingFlashbackTables(found)
	if len(got) != 5 {
		t.Fatalf("missing=%v want 5", got)
	}
	joined := strings.Join(got, ",")
	if !strings.Contains(joined, "tbl_flashback_logs") || !strings.Contains(joined, "tbl_flashback_sqls") || !strings.Contains(joined, "tbl_flashback_args") || !strings.Contains(joined, "tbl_flashback_instances") || !strings.Contains(joined, "tbl_flashback_artifacts") {
		t.Fatalf("missing=%v", got)
	}
}

func TestSQLListWhere_kindAndOps(t *testing.T) {
	where, args, next := sqlListWhere("t1", "undo", []string{"delete", "DELETE", " update "})
	if !strings.Contains(where, "AND kind=$2") || !strings.Contains(where, "upper(op) IN ($3,$4)") {
		t.Fatalf("where=%s", where)
	}
	if next != 5 || len(args) != 4 {
		t.Fatalf("next=%d args=%v", next, args)
	}
	if args[0] != "t1" || args[1] != "undo" || args[2] != "DELETE" || args[3] != "UPDATE" {
		t.Fatalf("args=%v", args)
	}
	where, args, next = sqlListWhere("t1", "", nil)
	if where != "WHERE task_id=$1" || next != 2 || len(args) != 1 {
		t.Fatalf("empty filter where=%s next=%d args=%v", where, next, args)
	}
}

func TestCollectMissingFlashbackTaskCols(t *testing.T) {
	found := map[string]bool{
		"start_file": true, "start_pos": true, "stop_file": true, "stop_pos": true,
		"log_total": true, "log_done": true, "parse_total": true, "parse_done": true,
		"engine": true, "extra": true,
	}
	if got := collectMissingFlashbackTaskCols(found); len(got) != 0 {
		t.Fatalf("missing=%v want none", got)
	}
	got := collectMissingFlashbackTaskCols(map[string]bool{"start_file": true})
	if len(got) != 9 {
		t.Fatalf("missing=%v want 9", got)
	}
}

func TestFlashbackSchemaNotReadyErr(t *testing.T) {
	err := flashbackSchemaNotReadyErr([]string{"tbl_flashback_tasks"})
	if err == nil {
		t.Fatal("expected error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "tbl_flashback_tasks") || !strings.Contains(msg, "change/sql/") {
		t.Fatalf("msg=%q", msg)
	}
	if strings.Contains(msg, "tbl_flashback_tasks") && strings.Contains(msg, "CREATE TABLE tbl_flashback_tasks") {
		t.Fatal("must not suggest runtime CREATE for task tables")
	}
}

func TestFlashbackSchemaColumnsNotReadyErr(t *testing.T) {
	err := flashbackSchemaColumnsNotReadyErr([]string{"start_file"})
	if err == nil {
		t.Fatal("expected error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "start_file") || !strings.Contains(msg, "tbl_flashback_alter_binlog_pos.sql") {
		t.Fatalf("msg=%q", msg)
	}
	progressErr := flashbackSchemaColumnsNotReadyErr([]string{"log_total", "parse_done"})
	if progressErr == nil {
		t.Fatal("expected error")
	}
	pmsg := progressErr.Error()
	if !strings.Contains(pmsg, "log_total") || !strings.Contains(pmsg, "tbl_flashback_alter_progress.sql") {
		t.Fatalf("progress msg=%q", pmsg)
	}
	if strings.Contains(pmsg, "tbl_flashback_alter_binlog_pos.sql") {
		t.Fatalf("progress-only missing should not mention binlog alter: %q", pmsg)
	}
	pduErr := flashbackSchemaColumnsNotReadyErr([]string{"engine", "extra"})
	if pduErr == nil || !strings.Contains(pduErr.Error(), "tbl_flashback_pdu.sql") {
		t.Fatalf("pdu msg=%v", pduErr)
	}
}
