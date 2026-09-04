package service

import (
	"testing"
	"time"

	"db-flashback/internal/storage/flashback"
)

func TestFlashbackPageCatalogDashboardFirst(t *testing.T) {
	p := FlashbackPageCatalog()
	if len(p) == 0 || p[0].Key != PageDashboard {
		t.Fatalf("dashboard should be first: %+v", p)
	}
}

func TestFlashbackDeltaPct(t *testing.T) {
	if flashbackDeltaPct(0, 0) != nil {
		t.Fatal("empty should be nil")
	}
	if v := flashbackDeltaPct(4, 0); v == nil || *v != 100 {
		t.Fatalf("from zero: %v", v)
	}
	v := flashbackDeltaPct(12, 10)
	if v == nil || *v < 19.9 || *v > 20.1 {
		t.Fatalf("20%%: %v", v)
	}
}

func TestFlashbackSuccessRate(t *testing.T) {
	if flashbackSuccessRate(43, 47) < 91.4 || flashbackSuccessRate(43, 47) > 91.5 {
		t.Fatalf("rate=%v", flashbackSuccessRate(43, 47))
	}
	if flashbackSuccessRate(0, 0) != 0 {
		t.Fatal("empty rate")
	}
}

func TestFlashbackFillDays(t *testing.T) {
	loc := time.FixedZone("CST", 8*3600)
	start := time.Date(2026, 8, 29, 0, 0, 0, 0, loc) // Saturday
	rows := []flashback.DashboardDayRow{{
		Day: time.Date(2026, 8, 31, 0, 0, 0, 0, loc), Succeeded: 3, Failed: 1,
	}}
	days := flashbackFillDays(start, loc, rows)
	if len(days) != 7 || days[0].Label != "六" || days[6].Label != "五" {
		t.Fatalf("labels: %+v", days)
	}
	if days[2].Date != "2026-08-31" || days[2].Succeeded != 3 || days[2].Failed != 1 {
		t.Fatalf("monday: %+v", days[2])
	}
	if days[0].Succeeded != 0 {
		t.Fatal("missing day should be zero")
	}
}

func TestHasPagePermDashboardAdmin(t *testing.T) {
	if !HasPagePerm("admin", "{}", PageDashboard, PermOperate) {
		t.Fatal("admin should operate dashboard")
	}
	if HasPagePerm("dba1", `{"tasks":"view"}`, PageDashboard, PermView) {
		t.Fatal("no dashboard grant")
	}
}
