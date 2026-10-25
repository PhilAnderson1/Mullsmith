// Package store is the SQLite persistence layer: migrations plus typed
// queries. All SQL lives here; parameterized statements only (spec §9.3).
package store

import (
	"database/sql"
	"embed"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite" // pure-Go driver — CGO_ENABLED=0 builds stay static
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// DB wraps the sql database handle.
type DB struct {
	sqlDB *sql.DB
}

// Open opens (creating if needed) the database file and applies migrations.
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
	if err := d.migrate(); err != nil {
		_ = sqlDB.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return d, nil
}

func (d *DB) Close() error { return d.sqlDB.Close() }

func (d *DB) migrate() error {
	if _, err := d.sqlDB.Exec(`CREATE TABLE IF NOT EXISTS schema_version (
		version INTEGER PRIMARY KEY, applied_at INTEGER NOT NULL)`); err != nil {
		return err
	}
	var cur int
	if err := d.sqlDB.QueryRow(`SELECT COALESCE(MAX(version), 0) FROM schema_version`).Scan(&cur); err != nil {
		return err
	}

	entries, err := migrationsFS.ReadDir("migrations")
	if err != nil {
		return err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	// names arrive sorted from ReadDir.
	for _, name := range names {
		var ver int
		if _, err := fmt.Sscanf(name, "%04d_", &ver); err != nil {
			return fmt.Errorf("migration name %q: expected NNNN_description.sql", name)
		}
		if ver <= cur {
			continue
		}
		body, err := migrationsFS.ReadFile("migrations/" + name)
		if err != nil {
			return err
		}
		tx, err := d.sqlDB.Begin()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(string(body)); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("migration %s: %w", name, err)
		}
		if _, err := tx.Exec(`INSERT INTO schema_version(version, applied_at) VALUES(?, ?)`,
			ver, time.Now().Unix()); err != nil {
			_ = tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
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
