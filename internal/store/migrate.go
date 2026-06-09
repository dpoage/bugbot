package store

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
	"sort"
	"strconv"
	"strings"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// migration is a single versioned schema change. Files are named NNN_name.sql
// where NNN is a zero-padded integer version applied in ascending order.
type migration struct {
	version int
	name    string
	sql     string
}

// loadMigrations reads and parses every embedded migration file, returning them
// sorted by ascending version. It errors on malformed filenames or duplicate
// versions so a packaging mistake fails loudly at startup rather than silently
// skipping a migration.
func loadMigrations() ([]migration, error) {
	entries, err := fs.ReadDir(migrationsFS, "migrations")
	if err != nil {
		return nil, fmt.Errorf("read embedded migrations: %w", err)
	}

	var migs []migration
	seen := make(map[int]string)
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".sql") {
			continue
		}
		// Expect NNN_description.sql.
		prefix, _, ok := strings.Cut(strings.TrimSuffix(name, ".sql"), "_")
		if !ok {
			return nil, fmt.Errorf("migration %q: name must be NNN_description.sql", name)
		}
		version, err := strconv.Atoi(prefix)
		if err != nil {
			return nil, fmt.Errorf("migration %q: leading version must be an integer: %w", name, err)
		}
		if prev, dup := seen[version]; dup {
			return nil, fmt.Errorf("migration %q: duplicate version %d (also %q)", name, version, prev)
		}
		seen[version] = name

		body, err := fs.ReadFile(migrationsFS, "migrations/"+name)
		if err != nil {
			return nil, fmt.Errorf("read migration %q: %w", name, err)
		}
		migs = append(migs, migration{version: version, name: name, sql: string(body)})
	}

	sort.Slice(migs, func(i, j int) bool { return migs[i].version < migs[j].version })
	return migs, nil
}

// migrate applies every migration whose version is greater than the current
// schema version. Each migration runs in its own transaction together with the
// schema_migrations bookkeeping insert, so a failed migration leaves the DB at
// the last successfully applied version. Re-running with no pending migrations
// is a no-op, making Open idempotent.
func migrate(ctx context.Context, db *sql.DB) error {
	if _, err := db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version    INTEGER PRIMARY KEY,
			name       TEXT NOT NULL,
			applied_at TEXT NOT NULL
		)`); err != nil {
		return fmt.Errorf("create schema_migrations: %w", err)
	}

	var current int
	if err := db.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(version), 0) FROM schema_migrations`).Scan(&current); err != nil {
		return fmt.Errorf("read current schema version: %w", err)
	}

	migs, err := loadMigrations()
	if err != nil {
		return err
	}

	for _, m := range migs {
		if m.version <= current {
			continue
		}
		if err := applyMigration(ctx, db, m); err != nil {
			return fmt.Errorf("apply migration %s: %w", m.name, err)
		}
	}
	return nil
}

func applyMigration(ctx context.Context, db *sql.DB, m migration) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, m.sql); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO schema_migrations (version, name, applied_at) VALUES (?, ?, ?)`,
		m.version, m.name, nowUTC().Format(timeLayout),
	); err != nil {
		return err
	}
	return tx.Commit()
}
