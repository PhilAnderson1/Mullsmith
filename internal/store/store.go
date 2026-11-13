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

// migrationV1SQL is the immutable initial schema. Once released, migrations
// must be appended rather than changing SQL that an older database has already
// recorded as applied.
//
//go:embed schema/schema.sql
var migrationV1SQL string

//go:embed schema/002_reasoning.sql
var migrationV2SQL string

//go:embed schema/003_generation_timings.sql
var migrationV3SQL string

const currentSchemaVersion = 3

// schemaMigrations is indexed by target version: element 0 migrates version
// 0 to version 1. Keep old entries immutable and append each new migration.
var schemaMigrations = [...]string{
	migrationV1SQL,
	migrationV2SQL,
	migrationV3SQL,
}

// DB wraps the sql database handle.
type DB struct {
	sqlDB                   *sql.DB
	maxConversationsPerUser int
	maxMessagesPerUser      int
}

// Open opens the database file and migrates it to the current schema if needed.
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
	if err := d.migrateSchema(); err != nil {
		_ = sqlDB.Close()
		return nil, fmt.Errorf("migrate schema: %w", err)
	}
	return d, nil
}

func (d *DB) Close() error { return d.sqlDB.Close() }

// migrateSchema applies every outstanding migration and its version marker in
// one transaction. Version 0 is reserved for a new, completely empty database;
// pre-versioning development databases are intentionally not supported.
func (d *DB) migrateSchema() error {
	var version int
	if err := d.sqlDB.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		return fmt.Errorf("read schema version: %w", err)
	}
	if version > currentSchemaVersion {
		return fmt.Errorf("database schema version %d is newer than this application supports (version %d)", version, currentSchemaVersion)
	}
	if version == currentSchemaVersion {
		return nil
	}
	if len(schemaMigrations) != currentSchemaVersion {
		return fmt.Errorf("internal migration table has %d entries for schema version %d", len(schemaMigrations), currentSchemaVersion)
	}
	if version == 0 {
		var objects int
		if err := d.sqlDB.QueryRow(`
			SELECT COUNT(*)
			FROM sqlite_schema
			WHERE name NOT LIKE 'sqlite_%'
		`).Scan(&objects); err != nil {
			return fmt.Errorf("inspect unversioned database: %w", err)
		}
		if objects != 0 {
			return fmt.Errorf("database is unversioned but not empty; recreate it before starting this release")
		}
	}

	tx, err := d.sqlDB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for target := version + 1; target <= currentSchemaVersion; target++ {
		if _, err := tx.Exec(schemaMigrations[target-1]); err != nil {
			return fmt.Errorf("apply schema migration %d: %w", target, err)
		}
		if _, err := tx.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, target)); err != nil {
			return fmt.Errorf("record schema migration %d: %w", target, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit schema migrations: %w", err)
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
