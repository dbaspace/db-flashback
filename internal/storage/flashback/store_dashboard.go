package flashback

import (
	"context"
	"fmt"
	"time"
)

type DashboardCounts struct {
	Total       int
	Succeeded   int
	Failed      int
	Pending     int
	WALBytes    int64
	Today       int
	Yesterday   int
	WeekWAL     int64
	PrevWeekWAL int64
}

type DashboardDayRow struct {
	Day       time.Time
	Succeeded int
	Failed    int
}

func (s Store) DashboardCounts(ctx context.Context, todayStart, yesterdayStart, weekStart time.Time) (DashboardCounts, error) {
	var out DashboardCounts
	db := s.db()
	if db == nil {
		return out, fmt.Errorf("database not initialized")
	}
	err := db.QueryRowContext(ctx, `
SELECT
  COUNT(*),
  COUNT(*) FILTER (WHERE status = 'succeeded'),
  COUNT(*) FILTER (WHERE status = 'failed'),
  COUNT(*) FILTER (WHERE status IN ('pending', 'running')),
  COALESCE(SUM(wal_bytes) FILTER (WHERE status = 'succeeded'), 0),
  COUNT(*) FILTER (WHERE created_at >= $1),
  COUNT(*) FILTER (WHERE created_at >= $2 AND created_at < $1),
  COALESCE(SUM(wal_bytes) FILTER (WHERE status = 'succeeded' AND created_at >= $3), 0),
  COALESCE(SUM(wal_bytes) FILTER (WHERE status = 'succeeded' AND created_at >= $3 - INTERVAL '7 days' AND created_at < $3), 0)
FROM tbl_flashback_tasks`, todayStart, yesterdayStart, weekStart).Scan(
		&out.Total, &out.Succeeded, &out.Failed, &out.Pending, &out.WALBytes,
		&out.Today, &out.Yesterday, &out.WeekWAL, &out.PrevWeekWAL,
	)
	return out, err
}

func (s Store) DashboardDays(ctx context.Context, since time.Time) ([]DashboardDayRow, error) {
	db := s.db()
	if db == nil {
		return nil, fmt.Errorf("database not initialized")
	}
	rows, err := db.QueryContext(ctx, `
SELECT (created_at AT TIME ZONE 'Asia/Shanghai')::date AS d,
       COUNT(*) FILTER (WHERE status = 'succeeded'),
       COUNT(*) FILTER (WHERE status = 'failed')
FROM tbl_flashback_tasks
WHERE created_at >= $1
GROUP BY 1
ORDER BY 1`, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DashboardDayRow
	for rows.Next() {
		var r DashboardDayRow
		if err := rows.Scan(&r.Day, &r.Succeeded, &r.Failed); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
