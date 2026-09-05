package database

import (
	"context"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/IUCLabs/golib/pg"

	"github.com/IUCLabs/artifactapi/migrations"
)

// migrationsDir is the repo's migrations/ directory, relative to this package.
const migrationsDir = "../../migrations"

func readMigrationsFromDisk(t *testing.T) map[string]string {
	t.Helper()
	entries, err := os.ReadDir(migrationsDir)
	if err != nil {
		t.Fatalf("read %s: %v", migrationsDir, err)
	}
	files := map[string]string{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(migrationsDir, e.Name()))
		if err != nil {
			t.Fatalf("read %s: %v", e.Name(), err)
		}
		files[e.Name()] = string(b)
	}
	if len(files) == 0 {
		t.Fatalf("no .sql files in %s", migrationsDir)
	}
	return files
}

// The embedded set is the shipped schema, so it must match the migrations/
// directory exactly — a file added on disk but not embedded would never run.
func TestEmbeddedMigrationsMatchDirectory(t *testing.T) {
	onDisk := readMigrationsFromDisk(t)
	entries, err := fs.ReadDir(migrations.FS, ".")
	if err != nil {
		t.Fatalf("read embedded migrations: %v", err)
	}
	embedded := map[string]string{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		b, err := migrations.FS.ReadFile(e.Name())
		if err != nil {
			t.Fatalf("read embedded %s: %v", e.Name(), err)
		}
		embedded[e.Name()] = string(b)
	}
	if len(embedded) != len(onDisk) {
		t.Fatalf("embedded %d files, migrations/ has %d", len(embedded), len(onDisk))
	}
	for name, body := range embedded {
		want, ok := onDisk[name]
		if !ok {
			t.Errorf("%s is embedded but not in migrations/", name)
			continue
		}
		if body != want {
			t.Errorf("%s: embedded body differs from migrations/%s", name, name)
		}
	}
}

var (
	createTableRe = regexp.MustCompile(`(?i)\bCREATE\s+TABLE\b(\s+IF\s+NOT\s+EXISTS\b)?`)
	createIndexRe = regexp.MustCompile(`(?i)\bCREATE\s+(UNIQUE\s+)?INDEX\b(\s+IF\s+NOT\s+EXISTS\b)?`)
	addColumnRe   = regexp.MustCompile(`(?i)\bADD\s+COLUMN\b(\s+IF\s+NOT\s+EXISTS\b)?`)
	destructiveRe = regexp.MustCompile(`(?i)\b(DROP\s+(TABLE|COLUMN|INDEX)|TRUNCATE|DELETE\s+FROM)\b`)
)

// A migration absent from schema_migrations is re-run even when the live
// database already has the schema, which is exactly how the deployed database —
// migrated for years by an untracked inline DDL blob — picks 0001 up. Every
// statement must therefore be idempotent, or that first tracked run would fail
// against production.
func TestMigrationsAreIdempotent(t *testing.T) {
	for name, body := range readMigrationsFromDisk(t) {
		for _, m := range createTableRe.FindAllStringSubmatch(body, -1) {
			if m[1] == "" {
				t.Errorf("%s: %q is not IF NOT EXISTS-guarded", name, strings.Join(strings.Fields(m[0]), " "))
			}
		}
		for _, m := range createIndexRe.FindAllStringSubmatch(body, -1) {
			if m[2] == "" {
				t.Errorf("%s: %q is not IF NOT EXISTS-guarded", name, strings.Join(strings.Fields(m[0]), " "))
			}
		}
		for _, m := range addColumnRe.FindAllStringSubmatch(body, -1) {
			if m[1] == "" {
				t.Errorf("%s: %q is not IF NOT EXISTS-guarded", name, strings.Join(strings.Fields(m[0]), " "))
			}
		}
		if loc := destructiveRe.FindString(body); loc != "" {
			t.Errorf("%s: destructive statement %q; migrations are additive", name, loc)
		}
	}
}

// The deployed database was built by the untracked inline DDL this runner
// replaces, so its very first tracked start runs 0001 against a schema that
// already exists. Reproduce that by dropping the tracking row from an
// already-migrated database and starting again: it must succeed and re-record
// the version, changing nothing else.
func TestMigratingAnAlreadyPopulatedSchemaIsANoOp(t *testing.T) {
	requireDB(t)
	c := context.Background()

	if _, err := testDB.Pool.Exec(c, "DELETE FROM schema_migrations"); err != nil {
		t.Fatalf("clear schema_migrations: %v", err)
	}
	db, err := New(testDSN)
	if err != nil {
		t.Fatalf("migrate over an existing schema: %v", err)
	}
	defer db.Close()

	var versions []string
	rows, err := db.Pool.Query(c, "SELECT version FROM schema_migrations ORDER BY version")
	if err != nil {
		t.Fatalf("read schema_migrations: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			t.Fatalf("scan version: %v", err)
		}
		versions = append(versions, v)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read schema_migrations: %v", err)
	}

	want := slices.Sorted(maps.Keys(readMigrationsFromDisk(t)))
	if !slices.Equal(versions, want) {
		t.Fatalf("schema_migrations = %v, want %v", versions, want)
	}
}

// The advisory lock key is derived from migrationLockName by golib. Pin it so a
// rename cannot silently let two builds migrate the same cluster at once.
func TestMigrationLockKeyIsPinned(t *testing.T) {
	const wantKey int64 = -6981019939451326383
	if got := pg.LockKey(migrationLockName); got != wantKey {
		t.Fatalf("LockKey(%q) = %d, want %d", migrationLockName, got, wantKey)
	}
}
