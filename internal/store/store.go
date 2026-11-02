// Package store is the SQLite persistence layer: schema plus typed
// queries. All SQL lives here; parameterized statements only (spec §9.3).
package store

import (
	"database/sql"
	_ "embed"
	"fmt"
	"path/filepath"
	"strings"

	_ "modernc.org/sqlite" // pure-Go driver — CGO_ENABLED=0 builds stay static
)

//go:embed schema/schema.sql
var schemaSQL string

// DB wraps the sql database handle.
type DB struct {
	sqlDB *sql.DB
}

// Open opens the database file and initializes the current schema if needed.
func Open(path string) (*DB, error) {
	dsn := fmt.Sprintf("file:%s?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)&_pragma=data_sync(NORMAL)",
		filepath.Clean(path))
	sqlDB, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	// Single connection serialises writers and keeps SQLite happy at v1
	// scale (spec §9.11); revisit with a reader pool if ever measurable.
	sqlDB.SetMaxOpenConns(1)
	d := &DB{sqlDB: sqlDB}
	if err := sqlDB.Ping(); err != nil {
		_ = sqlDB.Close()
		return nil, fmt.Errorf("ping database: %w", err)
	}
	if err := d.initializeSchema(); err != nil {
		_ = sqlDB.Close()
		return nil, fmt.Errorf("initialize schema: %w", err)
	}
	return d, nil
}

func (d *DB) Close() error { return d.sqlDB.Close() }

// initializeSchema applies the current schema atomically. This development
// application does not upgrade historical database schemas.
func (d *DB) initializeSchema() error {
	tx, err := d.sqlDB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(schemaSQL); err != nil {
		return err
	}
	return tx.Commit()
}

// ErrNoRows aliases sql.ErrNoRows for row lookups.
var ErrNoRows = sql.ErrNoRows

// row is the shared interface for QueryRow results.
type row interface{ Scan(dest ...any) error }

// isUnique reports whether err is a SQLite UNIQUE constraint violation on the
// named column.
func isUnique(err error, column string) bool {
	return err != nil &&
		strings.Contains(err.Error(), "UNIQUE constraint failed") &&
		strings.Contains(err.Error(), column)
}
