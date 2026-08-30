// Command bay serves the Bay allocator over unary gRPC.
// Internal service: only Booking calls it. Never binds :8080 (Gateway in
// Phase 3) and never calls Catalog.
package main

import (
	"context"
	"flag"
	"log"
	"net"
	"os"

	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/grpc"

	"github.com/athandoan/unified-service-scheduler/services/bay"
	bayv1 "github.com/athandoan/unified-service-scheduler/shared/gen/proto/bay/v1"
	"github.com/athandoan/unified-service-scheduler/shared/pkg/reaper"
)

func main() {
	addr := flag.String("addr", envOr("BAY_ADDR", ":9092"),
		"listen address (never :8080 — that is the Gateway)")
	dsn := flag.String("dsn", envOr("BAY_DATABASE_URL",
		"postgres://scheduler:scheduler@localhost:5432/bay?sslmode=disable"),
		"bay Postgres DSN")
	flag.Parse()

	pool, err := pgxpool.New(context.Background(), *dsn)
	if err != nil {
		log.Fatalf("bay: connect: %v", err)
	}
	if err := pool.Ping(context.Background()); err != nil {
		log.Fatalf("bay: ping: %v", err)
	}

	lis, err := net.Listen("tcp", *addr)
	if err != nil {
		log.Fatalf("bay: listen %s: %v", *addr, err)
	}
	reaper.Start(context.Background(), pool, "bay_occupation", "bay")
	srv := grpc.NewServer()
	bayv1.RegisterBayServiceServer(srv, bay.NewServer(pool))
	log.Printf("bay: gRPC listening on %s", *addr)
	if err := srv.Serve(lis); err != nil {
		log.Fatalf("bay: serve: %v", err)
	}
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
