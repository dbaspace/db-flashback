package service

import "testing"

func TestHasPagePerm(t *testing.T) {
	if !HasPagePerm("admin", "{}", PageTasks, PermOperate) {
		t.Fatal("admin should operate all")
	}
	stored := `{"tasks":"view","instances":"operate","oops":"view"}`
	if !HasPagePerm("dba1", stored, PageTasks, PermView) || HasPagePerm("dba1", stored, PageTasks, PermOperate) {
		t.Fatal("tasks view only")
	}
	if !HasPagePerm("dba1", stored, PageInstances, PermView) || !HasPagePerm("dba1", stored, PageInstances, PermOperate) {
		t.Fatal("instances operate includes view")
	}
	if HasPagePerm("dba1", stored, PageHistory, PermView) || HasPagePerm("dba1", stored, "oops", PermView) {
		t.Fatal("unknown or missing page")
	}
}

func TestNormalizePerms(t *testing.T) {
	n := normalizePerms(map[string]string{"tasks": "OPERATE", "users": "view", "history": "nope"})
	if n[PageTasks] != PermOperate || n[PageHistory] != "" || len(n) != 1 {
		t.Fatalf("%v", n)
	}
}
