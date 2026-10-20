// Package testdb creates throwaway Postgres databases from the API's SQL
// migrations for integration tests. It is used only under the `integration`
// build tag and needs MP_TEST_DATABASE_URL to name a reachable server with
// CREATEDB rights (e.g. postgres://postgres@127.0.0.1:5432/postgres).
package testdb

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// MigrationsDir is where the API keeps its migrations, relative to the
// orchestrator module root.
const MigrationsDir = "../api/migrations"

// New returns a pool on a fresh database with every migration applied. The
// database is dropped when the test ends.
func New(t *testing.T) *pgxpool.Pool {
	t.Helper()
	base := os.Getenv("MP_TEST_DATABASE_URL")
	if base == "" {
		t.Skip("MP_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, base)
	if err != nil {
		t.Fatalf("connect %s: %v", base, err)
	}
	name := fmt.Sprintf("mp_go_%s", strings.ToLower(strings.NewReplacer("/", "_", " ", "_", "-", "_").Replace(t.Name())))
	if len(name) > 60 {
		name = name[:60]
	}
	_, _ = admin.Exec(ctx, "DROP DATABASE IF EXISTS "+name)
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatalf("create database: %v", err)
	}
	t.Cleanup(func() {
		_, _ = admin.Exec(context.Background(), "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
		_ = admin.Close(context.Background())
	})
	url := base[:strings.LastIndex(base, "/")] + "/" + name
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatalf("connect %s: %v", url, err)
	}
	t.Cleanup(pool.Close)

	dir, err := findMigrations()
	if err != nil {
		t.Fatal(err)
	}
	files, _ := filepath.Glob(filepath.Join(dir, "*.sql"))
	sort.Strings(files)
	if len(files) == 0 {
		t.Fatalf("no migrations in %s", dir)
	}
	for _, f := range files {
		sql, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, string(sql)); err != nil {
			t.Fatalf("apply %s: %v", filepath.Base(f), err)
		}
	}
	return pool
}

func findMigrations() (string, error) {
	dir, _ := os.Getwd()
	for i := 0; i < 6; i++ {
		candidate := filepath.Join(dir, "..", "api", "migrations")
		if st, err := os.Stat(candidate); err == nil && st.IsDir() {
			return filepath.Clean(candidate), nil
		}
		dir = filepath.Dir(dir)
	}
	return "", fmt.Errorf("apps/api/migrations not found above %s", dir)
}

// Seed inserts an organization, a published template and returns their ids.
func Seed(t *testing.T, pool *pgxpool.Pool) (orgID, templateID int64) {
	t.Helper()
	ctx := context.Background()
	if err := pool.QueryRow(ctx, `INSERT INTO organizations (name) VALUES ('t') RETURNING id`).Scan(&orgID); err != nil {
		t.Fatalf("seed org: %v", err)
	}
	if err := pool.QueryRow(ctx, `
        INSERT INTO rule_templates (org_id, template_key, version, name, schema_json, published)
        VALUES ($1, 'customers', 1, 'Customers', '{"id":"rt","version":1,"name":"Customers","source":{"type":"csv"},"mapping":{"payload":{}},"destination":{"type":"http","method":"POST","url":"http://x/y"}}', true)
        RETURNING id`, orgID).Scan(&templateID); err != nil {
		t.Fatalf("seed template: %v", err)
	}
	return orgID, templateID
}

// Job inserts a job in the given status and returns its id.
func Job(t *testing.T, pool *pgxpool.Pool, orgID, templateID int64, status, workflowID string) int64 {
	t.Helper()
	var id int64
	if err := pool.QueryRow(context.Background(), `
        INSERT INTO jobs (org_id, rule_template_id, rule_template_version, source_ref, status, temporal_workflow_id, started_at)
        VALUES ($1, $2, 1, '{"type":"minio","bucket":"b","key":"k"}', $3::job_status, $4, now())
        RETURNING id`, orgID, templateID, status, workflowID).Scan(&id); err != nil {
		t.Fatalf("seed job: %v", err)
	}
	return id
}
