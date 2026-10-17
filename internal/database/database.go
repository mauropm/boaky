// Package database opens the SQLite database, applies embedded migrations,
// and provides shared helpers. The driver is modernc.org/sqlite (pure Go,
// CGO_ENABLED=0) which ships SQLite with FTS5 enabled.
package database

import (
	"database/sql"
	"embed"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	_ "modernc.org/sqlite"
)

//go:embed migrations/*.sql
var migrations embed.FS

// Open opens (creating if needed) the SQLite database at path and applies
// pending migrations. It enforces WAL mode, foreign keys, and a busy timeout.
func Open(path string, poolSize int) (*sql.DB, error) {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("create db dir: %w", err)
		}
	}
	dsn := "file:" + filepath.ToSlash(path) +
		"?_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)" +
		"&_pragma=busy_timeout(5000)&_pragma=synchronous(NORMAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	if poolSize < 1 {
		poolSize = 1
	}
	db.SetMaxOpenConns(poolSize)
	db.SetMaxIdleConns(poolSize)
	db.SetConnMaxLifetime(0) // long-lived conns; WAL keeps them cheap
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("ping sqlite: %w", err)
	}
	if err := checkFTS5(db); err != nil {
		db.Close()
		return nil, err
	}
	if err := migrate(db); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

// checkFTS5 verifies the driver compiles SQLite with FTS5 by creating a
// throwaway virtual table, failing fast with a clear message instead of a
// runtime error on first search.
func checkFTS5(db *sql.DB) error {
	if _, err := db.Exec(`CREATE VIRTUAL TABLE temp.boaky_fts_check USING fts5(x)`); err != nil {
		return fmt.Errorf("this SQLite build lacks FTS5 support: %w", err)
	}
	db.Exec(`DROP TABLE temp.boaky_fts_check`)
	return nil
}

func migrate(db *sql.DB) error {
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS schema_version (version INTEGER NOT NULL PRIMARY KEY)`); err != nil {
		return fmt.Errorf("create schema_version: %w", err)
	}
	var v int
	if err := db.QueryRow(`SELECT COALESCE(MAX(version), 0) FROM schema_version`).Scan(&v); err != nil {
		return err
	}
	names, err := migrationNames()
	if err != nil {
		return err
	}
	for i, name := range names {
		n := i + 1
		if n <= v {
			continue
		}
		data, err := migrations.ReadFile("migrations/" + name)
		if err != nil {
			return fmt.Errorf("read migration %s: %w", name, err)
		}
		tx, err := db.Begin()
		if err != nil {
			return err
		}
		// Schema migrations are single-statement-per-file friendly: execute
		// each semicolon-terminated statement individually so SQLite's
		// implicit handling of VIRTUAL TABLE / triggers is predictable.
		for _, stmt := range splitStatements(string(data)) {
			if _, err := tx.Exec(stmt); err != nil {
				tx.Rollback()
				return fmt.Errorf("migration %s: %w\nstmt: %s", name, err, stmt)
			}
		}
		if _, err := tx.Exec(`INSERT INTO schema_version(version) VALUES (?)`, n); err != nil {
			tx.Rollback()
			return fmt.Errorf("record migration %d: %w", n, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit migration %s: %w", name, err)
		}
	}
	return nil
}

func migrationNames() ([]string, error) {
	entries, err := migrations.ReadDir("migrations")
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		names = append(names, e.Name())
	}
	sort.Strings(names)
	return names, nil
}

// splitStatements strips "--" comment lines and splits on ";\n" boundaries.
func splitStatements(s string) []string {
	var b strings.Builder
	var out []string
	for _, line := range strings.Split(s, "\n") {
		t := strings.TrimSpace(line)
		if t == "" || strings.HasPrefix(t, "--") {
			continue
		}
		b.WriteString(line)
		b.WriteString("\n")
		if strings.HasSuffix(t, ";") {
			if stmt := strings.TrimSpace(b.String()); stmt != "" {
				out = append(out, strings.TrimSuffix(stmt, ";"))
			}
			b.Reset()
		}
	}
	if stmt := strings.TrimSpace(b.String()); stmt != "" {
		out = append(out, stmt)
	}
	return out
}

// Version returns the current schema version.
func Version(db *sql.DB) (int, error) {
	var v int
	err := db.QueryRow(`SELECT COALESCE(MAX(version), 0) FROM schema_version`).Scan(&v)
	return v, err
}

// MustVersion returns the schema version or 0.
func MustVersion(db *sql.DB) int {
	v, _ := Version(db)
	return v
}
