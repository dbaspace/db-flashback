package flashback

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

const flashbackArtifactsDDL = `
CREATE TABLE IF NOT EXISTS tbl_flashback_artifacts (
    id         BIGSERIAL PRIMARY KEY,
    task_id    VARCHAR(64)  NOT NULL,
    kind       VARCHAR(16)  NOT NULL DEFAULT '',
    relpath    TEXT         NOT NULL DEFAULT '',
    bytes      BIGINT       NOT NULL DEFAULT 0,
    row_count  INT          NOT NULL DEFAULT 0,
    created_at TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_flashback_artifacts_task ON tbl_flashback_artifacts (task_id, id);
`

const flashbackPDUTaskColsDDL = `
ALTER TABLE tbl_flashback_tasks
    ADD COLUMN IF NOT EXISTS engine VARCHAR(16) NOT NULL DEFAULT 'native';
ALTER TABLE tbl_flashback_tasks
    ADD COLUMN IF NOT EXISTS extra TEXT NOT NULL DEFAULT '{}';
`

func (s Store) EnsurePDUSchema(ctx context.Context) error {
	db := s.db()
	if db == nil {
		return fmt.Errorf("database not initialized")
	}
	if _, err := db.ExecContext(ctx, flashbackArtifactsDDL); err != nil {
		return err
	}
	var reg sql.NullString
	if err := db.QueryRowContext(ctx, `SELECT to_regclass($1)::text`, "public.tbl_flashback_tasks").Scan(&reg); err != nil {
		return err
	}
	if !reg.Valid || strings.TrimSpace(reg.String) == "" {
		return nil
	}
	_, err := db.ExecContext(ctx, flashbackPDUTaskColsDDL)
	return err
}

func (s Store) InsertArtifact(ctx context.Context, r *ArtifactRow) error {
	if r == nil || strings.TrimSpace(r.TaskID) == "" {
		return fmt.Errorf("artifact task_id is empty")
	}
	db := s.db()
	if db == nil {
		return fmt.Errorf("database not initialized")
	}
	return db.QueryRowContext(ctx, `
INSERT INTO tbl_flashback_artifacts (task_id, kind, relpath, bytes, row_count)
VALUES ($1,$2,$3,$4,$5) RETURNING id`,
		r.TaskID, r.Kind, r.RelPath, r.Bytes, r.RowCount).Scan(&r.ID)
}

func (s Store) ListArtifacts(ctx context.Context, taskID string) ([]*ArtifactRow, error) {
	db := s.db()
	if db == nil {
		return nil, fmt.Errorf("database not initialized")
	}
	rows, err := db.QueryContext(ctx, `
SELECT id, task_id, kind, relpath, bytes, row_count, created_at
FROM tbl_flashback_artifacts WHERE task_id=$1 ORDER BY id`, strings.TrimSpace(taskID))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*ArtifactRow
	for rows.Next() {
		r := &ArtifactRow{}
		if err := rows.Scan(&r.ID, &r.TaskID, &r.Kind, &r.RelPath, &r.Bytes, &r.RowCount, &r.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
