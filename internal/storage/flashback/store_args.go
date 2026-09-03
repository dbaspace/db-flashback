package flashback

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// ArgRow 多云参数一行。
type ArgRow struct {
	Key         string
	Value       string
	Description string
}

func (s Store) GetArg(ctx context.Context, key string) (string, error) {
	db := s.db()
	if db == nil {
		return "", fmt.Errorf("database not initialized")
	}
	key = strings.TrimSpace(key)
	if key == "" {
		return "", nil
	}
	var val string
	err := db.QueryRowContext(ctx, `SELECT value FROM tbl_flashback_args WHERE key=$1`, key).Scan(&val)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return val, nil
}

func (s Store) ListArgs(ctx context.Context) ([]ArgRow, error) {
	db := s.db()
	if db == nil {
		return nil, fmt.Errorf("database not initialized")
	}
	rows, err := db.QueryContext(ctx, `SELECT key, value, description FROM tbl_flashback_args ORDER BY key`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ArgRow
	for rows.Next() {
		var r ArgRow
		if err := rows.Scan(&r.Key, &r.Value, &r.Description); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s Store) UpsertArg(ctx context.Context, key, value, description string) error {
	db := s.db()
	if db == nil {
		return fmt.Errorf("database not initialized")
	}
	key = strings.TrimSpace(key)
	if key == "" {
		return fmt.Errorf("key 不能为空")
	}
	_, err := db.ExecContext(ctx, `
INSERT INTO tbl_flashback_args (key, value, description, updated_at)
VALUES ($1, $2, $3, NOW())
ON CONFLICT (key) DO UPDATE SET
  value = EXCLUDED.value,
  description = EXCLUDED.description,
  updated_at = NOW()`, key, value, strings.TrimSpace(description))
	return err
}
