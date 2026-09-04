package service

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"

	"db-flashback/internal/service/dto"
	"db-flashback/internal/storage/databases"
	"db-flashback/internal/storage/flashback"
)

var flashbackWeekdayCN = []string{"日", "一", "二", "三", "四", "五", "六"}

func flashbackShanghai() *time.Location {
	loc, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		return time.Local
	}
	return loc
}

func flashbackDeltaPct(cur, prev int64) *float64 {
	if prev <= 0 {
		if cur <= 0 {
			return nil
		}
		v := 100.0
		return &v
	}
	v := (float64(cur-prev) / float64(prev)) * 100
	return &v
}

func flashbackSuccessRate(ok, total int) float64 {
	if total <= 0 {
		return 0
	}
	return float64(ok) * 100 / float64(total)
}

func (s *FlashbackImpl) Dashboard(c *gin.Context) (*dto.FlashbackDashboard, error) {
	if err := s.ensureReady(c.Request.Context()); err != nil {
		return nil, err
	}
	loc := flashbackShanghai()
	now := time.Now().In(loc)
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc)
	yesterday := today.AddDate(0, 0, -1)
	weekStart := today.AddDate(0, 0, -6)

	counts, err := s.store.DashboardCounts(c.Request.Context(), today, yesterday, weekStart)
	if err != nil {
		return nil, err
	}
	dayRows, err := s.store.DashboardDays(c.Request.Context(), weekStart)
	if err != nil {
		return nil, err
	}
	recent, _, err := s.store.ListTasks(c.Request.Context(), flashback.TaskListFilter{Offset: 0, Limit: 6})
	if err != nil {
		return nil, err
	}

	out := &dto.FlashbackDashboard{
		Total: counts.Total, Succeeded: counts.Succeeded, Failed: counts.Failed, Pending: counts.Pending,
		SuccessRate:    flashbackRound1(flashbackSuccessRate(counts.Succeeded, counts.Total)),
		TodayCount:     counts.Today,
		YesterdayCount: counts.Yesterday,
		TodayDeltaPct:  flashbackDeltaPct(int64(counts.Today), int64(counts.Yesterday)),
		WALBytes:       counts.WALBytes,
		WeekWALBytes:   counts.WeekWAL,
		PrevWeekWAL:    counts.PrevWeekWAL,
		WeekDeltaPct:   flashbackDeltaPct(counts.WeekWAL, counts.PrevWeekWAL),
		Days:           flashbackFillDays(weekStart, loc, dayRows),
		Recent:         flashbackRecentTasks(recent),
		Storage:        flashbackStorageStats(flashbackWorkDirBase(c.Request.Context())),
		Health:         flashbackHealthStats(c.Request.Context()),
	}
	return out, nil
}

func flashbackRound1(v float64) float64 {
	return float64(int(v*10+0.5)) / 10
}

func flashbackFillDays(start time.Time, loc *time.Location, rows []flashback.DashboardDayRow) []dto.FlashbackDashboardDay {
	by := map[string]flashback.DashboardDayRow{}
	for _, r := range rows {
		d := r.Day.In(loc)
		key := time.Date(d.Year(), d.Month(), d.Day(), 0, 0, 0, 0, loc).Format("2006-01-02")
		by[key] = r
	}
	out := make([]dto.FlashbackDashboardDay, 0, 7)
	for i := 0; i < 7; i++ {
		d := start.AddDate(0, 0, i)
		key := d.Format("2006-01-02")
		hit := by[key]
		out = append(out, dto.FlashbackDashboardDay{
			Date: key, Label: flashbackWeekdayCN[d.Weekday()],
			Succeeded: hit.Succeeded, Failed: hit.Failed,
		})
	}
	return out
}

func flashbackRecentTasks(rows []*flashback.TaskRow) []dto.FlashbackDashboardTask {
	out := make([]dto.FlashbackDashboardTask, 0, len(rows))
	for _, r := range rows {
		if r == nil {
			continue
		}
		var tables []string
		_ = json.Unmarshal([]byte(r.Tables), &tables)
		if tables == nil {
			tables = []string{}
		}
		out = append(out, dto.FlashbackDashboardTask{
			ID: r.ID, Tables: tables, Database: r.DatabaseName, Status: r.Status, CreatedAt: r.CreatedAt,
		})
	}
	return out
}

func flashbackStorageStats(workDir string) dto.FlashbackDashboardStorage {
	out := dto.FlashbackDashboardStorage{WorkDir: workDir}
	if workDir == "" {
		return out
	}
	_ = os.MkdirAll(workDir, 0o755)
	var st syscall.Statfs_t
	if err := syscall.Statfs(workDir, &st); err == nil {
		bsize := uint64(st.Bsize)
		out.TotalBytes = st.Blocks * bsize
		out.FreeBytes = st.Bavail * bsize
		if out.TotalBytes > out.FreeBytes {
			out.UsedBytes = out.TotalBytes - out.FreeBytes
		}
		if out.TotalBytes > 0 {
			out.UsedPercent = int((out.UsedBytes*100 + out.TotalBytes/2) / out.TotalBytes)
		}
	}
	out.WorkDirBytes = flashbackDirSize(workDir, 4000)
	return out
}

func flashbackDirSize(root string, limit int) uint64 {
	var n uint64
	var seen int
	_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info == nil {
			return nil
		}
		if !info.IsDir() {
			n += uint64(info.Size())
		}
		seen++
		if seen >= limit {
			return filepath.SkipAll
		}
		return nil
	})
	return n
}

func flashbackHealthStats(ctx context.Context) dto.FlashbackDashboardHealth {
	out := dto.FlashbackDashboardHealth{Available: true}
	db := databases.GetRawDB()
	if db == nil {
		out.Available = false
		return out
	}
	st := db.Stats()
	out.OpenConns = st.OpenConnections
	out.MaxConns = st.MaxOpenConnections
	start := time.Now()
	if err := db.PingContext(ctx); err != nil {
		out.Available = false
	}
	out.PingMS = int(time.Since(start).Milliseconds())
	if out.PingMS < 1 {
		out.PingMS = 1
	}
	return out
}
