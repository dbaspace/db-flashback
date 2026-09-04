package flashback

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"db-flashback/internal/crypto"
)

const flashbackInstancesDDL = `
CREATE TABLE IF NOT EXISTS tbl_flashback_instances (
    id                VARCHAR(64)  PRIMARY KEY,
    db_type           VARCHAR(32)  NOT NULL DEFAULT 'postgres',
    host              VARCHAR(255) NOT NULL,
    port              INT          NOT NULL DEFAULT 5432,
    db_user           VARCHAR(128) NOT NULL DEFAULT '',
    password          TEXT         NOT NULL DEFAULT '',
    sslmode           VARCHAR(32)  NOT NULL DEFAULT 'disable',
    vendor            VARCHAR(32)  NOT NULL DEFAULT '',
    cloud_instance_id VARCHAR(128) NOT NULL DEFAULT '',
    region            VARCHAR(64)  NOT NULL DEFAULT '',
    remark            VARCHAR(255) NOT NULL DEFAULT '',
    ssh_user          VARCHAR(128) NOT NULL DEFAULT '',
    ssh_port          INT          NOT NULL DEFAULT 0,
    updated_at        TIMESTAMPTZ  NOT NULL DEFAULT NOW()
)`

func (s Store) EnsureInstancesTable(ctx context.Context) error {
	db := s.db()
	if db == nil {
		return fmt.Errorf("database not initialized")
	}
	if _, err := db.ExecContext(ctx, flashbackInstancesDDL); err != nil {
		return err
	}
	for _, q := range []string{
		`ALTER TABLE tbl_flashback_instances ADD COLUMN IF NOT EXISTS ssh_user VARCHAR(128) NOT NULL DEFAULT ''`,
		`ALTER TABLE tbl_flashback_instances ADD COLUMN IF NOT EXISTS ssh_port INT NOT NULL DEFAULT 0`,
	} {
		if _, err := db.ExecContext(ctx, q); err != nil {
			return err
		}
	}
	return nil
}

// InstanceRow 闪回目标实例地址。
type InstanceRow struct {
	ID              string
	DBType          string
	Host            string
	Port            int
	User            string
	Password        string
	SSLMode         string
	Vendor          string
	CloudInstanceID string
	Region          string
	Remark          string
	SSHUser         string
	SSHPort         int
}

func (s Store) GetInstance(ctx context.Context, id string) (*InstanceRow, error) {
	db := s.db()
	if db == nil {
		return nil, fmt.Errorf("database not initialized")
	}
	id = strings.TrimSpace(id)
	if id == "" {
		return nil, nil
	}
	r := &InstanceRow{}
	err := db.QueryRowContext(ctx, `
SELECT id, db_type, host, port, db_user, password, sslmode, vendor, cloud_instance_id, region, remark, ssh_user, ssh_port
FROM tbl_flashback_instances WHERE id=$1`, id).Scan(
		&r.ID, &r.DBType, &r.Host, &r.Port, &r.User, &r.Password, &r.SSLMode,
		&r.Vendor, &r.CloudInstanceID, &r.Region, &r.Remark, &r.SSHUser, &r.SSHPort)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if err := decodeInstanceSecrets(r); err != nil {
		return nil, err
	}
	return r, nil
}

func (s Store) ListInstances(ctx context.Context) ([]InstanceRow, error) {
	db := s.db()
	if db == nil {
		return nil, fmt.Errorf("database not initialized")
	}
	rows, err := db.QueryContext(ctx, `
SELECT id, db_type, host, port, db_user, password, sslmode, vendor, cloud_instance_id, region, remark, ssh_user, ssh_port
FROM tbl_flashback_instances ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []InstanceRow
	for rows.Next() {
		var r InstanceRow
		if err := rows.Scan(&r.ID, &r.DBType, &r.Host, &r.Port, &r.User, &r.Password, &r.SSLMode,
			&r.Vendor, &r.CloudInstanceID, &r.Region, &r.Remark, &r.SSHUser, &r.SSHPort); err != nil {
			return nil, err
		}
		if err := decodeInstanceSecrets(&r); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s Store) ListInstancesRaw(ctx context.Context) ([]InstanceRow, error) {
	db := s.db()
	if db == nil {
		return nil, fmt.Errorf("database not initialized")
	}
	rows, err := db.QueryContext(ctx, `
SELECT id, db_type, host, port, db_user, password, sslmode, vendor, cloud_instance_id, region, remark, ssh_user, ssh_port
FROM tbl_flashback_instances ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []InstanceRow
	for rows.Next() {
		var r InstanceRow
		if err := rows.Scan(&r.ID, &r.DBType, &r.Host, &r.Port, &r.User, &r.Password, &r.SSLMode,
			&r.Vendor, &r.CloudInstanceID, &r.Region, &r.Remark, &r.SSHUser, &r.SSHPort); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s Store) UpsertInstance(ctx context.Context, r InstanceRow) error {
	db := s.db()
	if db == nil {
		return fmt.Errorf("database not initialized")
	}
	r.ID = strings.TrimSpace(r.ID)
	if r.ID == "" {
		return fmt.Errorf("id 不能为空")
	}
	if strings.TrimSpace(r.Host) == "" {
		return fmt.Errorf("host 不能为空")
	}
	if r.Port <= 0 {
		r.Port = 5432
	}
	if err := encodeInstanceSecrets(&r); err != nil {
		return err
	}
	_, err := db.ExecContext(ctx, `
INSERT INTO tbl_flashback_instances (
  id, db_type, host, port, db_user, password, sslmode, vendor, cloud_instance_id, region, remark, ssh_user, ssh_port, updated_at
) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,NOW())
ON CONFLICT (id) DO UPDATE SET
  db_type=EXCLUDED.db_type,
  host=EXCLUDED.host,
  port=EXCLUDED.port,
  db_user=EXCLUDED.db_user,
  password=CASE WHEN EXCLUDED.password = '' THEN tbl_flashback_instances.password ELSE EXCLUDED.password END,
  sslmode=EXCLUDED.sslmode,
  vendor=EXCLUDED.vendor,
  cloud_instance_id=EXCLUDED.cloud_instance_id,
  region=EXCLUDED.region,
  remark=EXCLUDED.remark,
  ssh_user=EXCLUDED.ssh_user,
  ssh_port=EXCLUDED.ssh_port,
  updated_at=NOW()`,
		r.ID, r.DBType, r.Host, r.Port, r.User, r.Password, r.SSLMode,
		r.Vendor, r.CloudInstanceID, r.Region, r.Remark, r.SSHUser, r.SSHPort)
	return err
}

func (s Store) DeleteInstance(ctx context.Context, id string) error {
	db := s.db()
	if db == nil {
		return fmt.Errorf("database not initialized")
	}
	id = strings.TrimSpace(id)
	if id == "" {
		return fmt.Errorf("id 不能为空")
	}
	res, err := db.ExecContext(ctx, `DELETE FROM tbl_flashback_instances WHERE id=$1`, id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("instance not found: %s", id)
	}
	return nil
}

func encodeInstanceSecrets(r *InstanceRow) error {
	if r == nil {
		return nil
	}
	user, err := crypto.MustSeal(strings.TrimSpace(r.User))
	if err != nil {
		return fmt.Errorf("加密实例用户: %w", err)
	}
	r.User = user
	if strings.TrimSpace(r.Password) == "" {
		return nil
	}
	pass, err := crypto.MustSeal(r.Password)
	if err != nil {
		return fmt.Errorf("加密实例密码: %w", err)
	}
	r.Password = pass
	return nil
}

func decodeInstanceSecrets(r *InstanceRow) error {
	if r == nil {
		return nil
	}
	user, err := crypto.Open(r.User)
	if err != nil {
		return fmt.Errorf("解密实例用户: %w", err)
	}
	r.User = user
	pass, err := crypto.Open(r.Password)
	if err != nil {
		return fmt.Errorf("解密实例密码: %w", err)
	}
	r.Password = pass
	return nil
}
