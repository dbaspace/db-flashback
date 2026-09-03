package databases

import (
	"database/sql"
	"fmt"
	"sync"

	_ "github.com/lib/pq"

	"db-flashback/internal/config"
)

var (
	rawDB   *sql.DB
	rawOnce sync.Once
	rawErr  error
)

func Open(cfg config.DBConfig) error {
	rawOnce.Do(func() {
		db, err := sql.Open("postgres", cfg.DSN())
		if err != nil {
			rawErr = err
			return
		}
		if err := db.Ping(); err != nil {
			_ = db.Close()
			rawErr = err
			return
		}
		rawDB = db
	})
	return rawErr
}

func GetRawDB() *sql.DB {
	return rawDB
}

func Close() error {
	if rawDB == nil {
		return nil
	}
	return rawDB.Close()
}

func MustOpen(cfg config.DBConfig) error {
	if err := Open(cfg); err != nil {
		return fmt.Errorf("open flashback meta db: %w", err)
	}
	if GetRawDB() == nil {
		return fmt.Errorf("open flashback meta db: empty connection")
	}
	return nil
}
