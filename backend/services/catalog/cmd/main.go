// Command catalog is the Catalog stub: read-only HTTP over the catalog
// database, matching openapi/catalog.yaml. Binds :8081 — never :8080 (that is
// the Gateway). Handlers live in the catalog package (../).
package main

import (
	"context"
	"flag"
	"log"
	"net/http"
	"os"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/athandoan/unified-service-scheduler/services/catalog"
)

func main() {
	addr := flag.String("addr", envOr("CATALOG_ADDR", ":8081"), "listen address (:8080 is the Gateway)")
	dsn := flag.String("dsn", envOr("CATALOG_DATABASE_URL",
		"postgres://scheduler:scheduler@localhost:5432/catalog?sslmode=disable"), "catalog database URL")
	flag.Parse()

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, *dsn)
	if err != nil {
		log.Fatalf("catalog: connect: %v", err)
	}
	if err := pool.Ping(ctx); err != nil {
		log.Fatalf("catalog: ping: %v", err)
	}

	log.Printf("catalog: listening on %s", *addr)
	if err := http.ListenAndServe(*addr, catalog.NewHandler(pool)); err != nil {
		log.Fatalf("catalog: serve: %v", err)
	}
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
