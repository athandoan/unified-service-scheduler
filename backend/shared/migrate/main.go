// Command migrate applies plain SQL migration files to the four scheduler
// databases (catalog, technician, bay, booking). Each service's migrations
// live in <service>/migrations/*.sql (folder-per-service layout) and are
// applied in filename order, each file exactly once (tracked in
// schema_migrations).
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"
)

var databases = []string{"catalog", "technician", "bay", "booking"}

func main() {
	host := flag.String("host", envOr("PGHOST", "localhost"), "postgres host")
	port := flag.String("port", envOr("PGPORT", "5432"), "postgres port")
	user := flag.String("user", envOr("PGUSER", "scheduler"), "postgres user")
	password := flag.String("password", envOr("PGPASSWORD", "scheduler"), "postgres password")
	root := flag.String("root", ".", "backend root containing <service>/migrations directories")
	flag.Parse()

	ctx := context.Background()
	failed := false
	for _, db := range databases {
		if err := migrateDatabase(ctx, *host, *port, *user, *password, db, *root); err != nil {
			failed = true
			fmt.Fprintf(os.Stderr, "migrate %s: %v\n", db, err)
		}
	}
	if failed {
		os.Exit(1)
	}
}

func migrateDatabase(ctx context.Context, host, port, user, password, db, root string) error {
	dsn := fmt.Sprintf("postgres://%s:%s@%s:%s/%s?sslmode=disable", user, password, host, port, db)
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer conn.Close(ctx)

	mdir := filepath.Join(root, "services", db, "migrations")
	files, err := migrationFiles(mdir)
	if err != nil {
		return err
	}

	if _, err := conn.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		filename text PRIMARY KEY,
		applied_at timestamptz NOT NULL DEFAULT now()
	)`); err != nil {
		return fmt.Errorf("create schema_migrations: %w", err)
	}

	for _, f := range files {
		var already bool
		if err := conn.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM schema_migrations WHERE filename = $1)`, f,
		).Scan(&already); err != nil {
			return err
		}
		if already {
			fmt.Printf("%-10s %-30s skip\n", db, f)
			continue
		}

		sql, err := os.ReadFile(filepath.Join(mdir, f))
		if err != nil {
			return err
		}
		tx, err := conn.Begin(ctx)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, string(sql)); err != nil {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("%s: %w", f, err)
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO schema_migrations (filename) VALUES ($1)`, f); err != nil {
			_ = tx.Rollback(ctx)
			return err
		}
		if err := tx.Commit(ctx); err != nil {
			return err
		}
		fmt.Printf("%-10s %-30s ok\n", db, f)
	}
	return nil
}

// migrationFiles lists .sql files in filename order; each must be NN_name.sql.
func migrationFiles(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", dir, err)
	}
	var files []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".sql") {
			continue
		}
		if len(name) < 3 || name[0] < '0' || name[0] > '9' {
			return nil, fmt.Errorf("migration %s must be named NN_name.sql", name)
		}
		files = append(files, name)
	}
	sort.Strings(files)
	return files, nil
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
