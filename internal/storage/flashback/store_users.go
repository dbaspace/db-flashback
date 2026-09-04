package flashback

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

const flashbackUsersDDL = `
CREATE TABLE IF NOT EXISTS tbl_flashback_users (
    username   VARCHAR(64) PRIMARY KEY,
    password   TEXT         NOT NULL,
    perms      TEXT         NOT NULL DEFAULT '{}',
    enabled          BOOLEAN      NOT NULL DEFAULT TRUE,
    locked           BOOLEAN      NOT NULL DEFAULT FALSE,
    login_fail_count INTEGER      NOT NULL DEFAULT 0,
    created_at       TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    updated_at       TIMESTAMPTZ  NOT NULL DEFAULT NOW()
)`

const flashbackSessionsDDL = `
CREATE TABLE IF NOT EXISTS tbl_flashback_sessions (
    token      VARCHAR(64) PRIMARY KEY,
    username   VARCHAR(64) NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
)`

func (s Store) EnsureUsersTable(ctx context.Context) error {
	db := s.db()
	if db == nil {
		return fmt.Errorf("database not initialized")
	}
	if _, err := db.ExecContext(ctx, flashbackUsersDDL); err != nil {
		return err
	}
	if _, err := db.ExecContext(ctx, flashbackSessionsDDL); err != nil {
		return err
	}
	for _, q := range []string{
		`ALTER TABLE tbl_flashback_users ADD COLUMN IF NOT EXISTS perms TEXT NOT NULL DEFAULT '{}'`,
		`ALTER TABLE tbl_flashback_users ADD COLUMN IF NOT EXISTS enabled BOOLEAN NOT NULL DEFAULT TRUE`,
		`ALTER TABLE tbl_flashback_users ADD COLUMN IF NOT EXISTS locked BOOLEAN NOT NULL DEFAULT FALSE`,
		`ALTER TABLE tbl_flashback_users ADD COLUMN IF NOT EXISTS login_fail_count INTEGER NOT NULL DEFAULT 0`,
		`CREATE INDEX IF NOT EXISTS idx_flashback_sessions_expires ON tbl_flashback_sessions (expires_at)`,
		`CREATE INDEX IF NOT EXISTS idx_flashback_sessions_user ON tbl_flashback_sessions (username)`,
	} {
		if _, err := db.ExecContext(ctx, q); err != nil {
			return err
		}
	}
	return nil
}

type UserRow struct {
	Username       string
	Password       string
	Perms          string
	Enabled        bool
	Locked         bool
	LoginFailCount int
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

func (s Store) CountUsers(ctx context.Context) (int, error) {
	db := s.db()
	if db == nil {
		return 0, fmt.Errorf("database not initialized")
	}
	var n int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM tbl_flashback_users`).Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}

func (s Store) GetUser(ctx context.Context, username string) (*UserRow, error) {
	db := s.db()
	if db == nil {
		return nil, fmt.Errorf("database not initialized")
	}
	username = strings.TrimSpace(username)
	if username == "" {
		return nil, nil
	}
	r := &UserRow{}
	err := db.QueryRowContext(ctx,
		`SELECT username, password, COALESCE(perms, '{}'), COALESCE(enabled, TRUE), COALESCE(locked, FALSE), COALESCE(login_fail_count, 0), created_at, updated_at FROM tbl_flashback_users WHERE lower(username)=lower($1)`,
		username).Scan(&r.Username, &r.Password, &r.Perms, &r.Enabled, &r.Locked, &r.LoginFailCount, &r.CreatedAt, &r.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return r, nil
}

func (s Store) ListUsers(ctx context.Context) ([]UserRow, error) {
	db := s.db()
	if db == nil {
		return nil, fmt.Errorf("database not initialized")
	}
	rows, err := db.QueryContext(ctx, `
SELECT username, COALESCE(perms, '{}'), COALESCE(enabled, TRUE), COALESCE(locked, FALSE), COALESCE(login_fail_count, 0), created_at, updated_at
FROM tbl_flashback_users ORDER BY username`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []UserRow
	for rows.Next() {
		var r UserRow
		if err := rows.Scan(&r.Username, &r.Perms, &r.Enabled, &r.Locked, &r.LoginFailCount, &r.CreatedAt, &r.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s Store) DeleteUser(ctx context.Context, username string) error {
	db := s.db()
	if db == nil {
		return fmt.Errorf("database not initialized")
	}
	res, err := db.ExecContext(ctx, `DELETE FROM tbl_flashback_users WHERE lower(username)=lower($1)`, strings.TrimSpace(username))
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("用户不存在")
	}
	return nil
}

func (s Store) InsertUser(ctx context.Context, username, passwordHash string) error {
	db := s.db()
	if db == nil {
		return fmt.Errorf("database not initialized")
	}
	_, err := db.ExecContext(ctx, `
INSERT INTO tbl_flashback_users (username, password, perms, created_at, updated_at)
VALUES ($1,$2,'{}',NOW(),NOW())`, strings.TrimSpace(username), passwordHash)
	return err
}

func (s Store) UpdateUserPerms(ctx context.Context, username, permsJSON string) error {
	db := s.db()
	if db == nil {
		return fmt.Errorf("database not initialized")
	}
	if strings.TrimSpace(permsJSON) == "" {
		permsJSON = "{}"
	}
	res, err := db.ExecContext(ctx, `
UPDATE tbl_flashback_users SET perms=$2, updated_at=NOW()
WHERE lower(username)=lower($1)`, strings.TrimSpace(username), permsJSON)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("用户不存在")
	}
	return nil
}

func (s Store) RecordLoginFail(ctx context.Context, username string, maxFail int) (int, bool, error) {
	db := s.db()
	if db == nil {
		return 0, false, fmt.Errorf("database not initialized")
	}
	if maxFail < 1 {
		maxFail = 3
	}
	var n int
	var locked bool
	err := db.QueryRowContext(ctx, `
UPDATE tbl_flashback_users
SET login_fail_count = login_fail_count + 1,
    locked = CASE WHEN login_fail_count + 1 >= $2 THEN TRUE ELSE locked END,
    updated_at = NOW()
WHERE lower(username)=lower($1)
RETURNING login_fail_count, locked`, strings.TrimSpace(username), maxFail).Scan(&n, &locked)
	if err == sql.ErrNoRows {
		return 0, false, fmt.Errorf("用户不存在")
	}
	if err != nil {
		return 0, false, err
	}
	return n, locked, nil
}

func (s Store) ResetLoginFail(ctx context.Context, username string) error {
	db := s.db()
	if db == nil {
		return fmt.Errorf("database not initialized")
	}
	_, err := db.ExecContext(ctx, `
UPDATE tbl_flashback_users SET login_fail_count=0, updated_at=NOW()
WHERE lower(username)=lower($1)`, strings.TrimSpace(username))
	return err
}

func (s Store) UnlockUser(ctx context.Context, username string) error {
	db := s.db()
	if db == nil {
		return fmt.Errorf("database not initialized")
	}
	res, err := db.ExecContext(ctx, `
UPDATE tbl_flashback_users SET locked=FALSE, login_fail_count=0, updated_at=NOW()
WHERE lower(username)=lower($1)`, strings.TrimSpace(username))
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("用户不存在")
	}
	return nil
}

func (s Store) SetUserEnabled(ctx context.Context, username string, enabled bool) error {
	db := s.db()
	if db == nil {
		return fmt.Errorf("database not initialized")
	}
	res, err := db.ExecContext(ctx, `
UPDATE tbl_flashback_users SET enabled=$2, updated_at=NOW()
WHERE lower(username)=lower($1)`, strings.TrimSpace(username), enabled)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("用户不存在")
	}
	return nil
}

func (s Store) UpdateUserPassword(ctx context.Context, username, passwordHash string) error {
	db := s.db()
	if db == nil {
		return fmt.Errorf("database not initialized")
	}
	res, err := db.ExecContext(ctx, `
UPDATE tbl_flashback_users SET password=$2, updated_at=NOW()
WHERE lower(username)=lower($1)`, strings.TrimSpace(username), passwordHash)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("用户不存在")
	}
	return nil
}

func (s Store) CreateSession(ctx context.Context, token, username string, expires time.Time) error {
	db := s.db()
	if db == nil {
		return fmt.Errorf("database not initialized")
	}
	_, err := db.ExecContext(ctx, `
INSERT INTO tbl_flashback_sessions (token, username, expires_at, created_at)
VALUES ($1,$2,$3,NOW())`, token, strings.TrimSpace(username), expires)
	return err
}

func (s Store) GetSessionUser(ctx context.Context, token string) (string, error) {
	db := s.db()
	if db == nil {
		return "", fmt.Errorf("database not initialized")
	}
	token = strings.TrimSpace(token)
	if token == "" {
		return "", nil
	}
	var username string
	err := db.QueryRowContext(ctx, `
SELECT username FROM tbl_flashback_sessions
WHERE token=$1 AND expires_at > NOW()`, token).Scan(&username)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return username, nil
}

func (s Store) DeleteSession(ctx context.Context, token string) error {
	db := s.db()
	if db == nil {
		return fmt.Errorf("database not initialized")
	}
	_, err := db.ExecContext(ctx, `DELETE FROM tbl_flashback_sessions WHERE token=$1`, strings.TrimSpace(token))
	return err
}

func (s Store) DeleteUserSessions(ctx context.Context, username string) error {
	db := s.db()
	if db == nil {
		return fmt.Errorf("database not initialized")
	}
	_, err := db.ExecContext(ctx, `DELETE FROM tbl_flashback_sessions WHERE lower(username)=lower($1)`, strings.TrimSpace(username))
	return err
}

func (s Store) DeleteExpiredSessions(ctx context.Context) error {
	db := s.db()
	if db == nil {
		return fmt.Errorf("database not initialized")
	}
	_, err := db.ExecContext(ctx, `DELETE FROM tbl_flashback_sessions WHERE expires_at <= NOW()`)
	return err
}
