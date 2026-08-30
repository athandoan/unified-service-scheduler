// Command technician serves the Technician allocator over unary gRPC.
// Internal service: only Booking calls it. Never binds :8080 (Gateway, Phase 3)
// and never calls Catalog.
package main

import (
	"context"
	"flag"
	"log"
	"net"
	"os"

	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/grpc"

	"github.com/athandoan/unified-service-scheduler/services/technician"
	techv1 "github.com/athandoan/unified-service-scheduler/shared/gen/proto/technician/v1"
	"github.com/athandoan/unified-service-scheduler/shared/pkg/reaper"
)

func main() {
	addr := flag.String("addr", envOr("TECHNICIAN_ADDR", ":9091"),
		"listen address (never :8080 — that is the Gateway)")
	dsn := flag.String("dsn", envOr("TECHNICIAN_DATABASE_URL",
		"postgres://scheduler:scheduler@localhost:5432/technician?sslmode=disable"),
		"technician Postgres DSN")
	flag.Parse()

	pool, err := pgxpool.New(context.Background(), *dsn)
	if err != nil {
		log.Fatalf("technician: connect: %v", err)
	}
	if err := pool.Ping(context.Background()); err != nil {
		log.Fatalf("technician: ping: %v", err)
	}

	lis, err := net.Listen("tcp", *addr)
	if err != nil {
		log.Fatalf("technician: listen %s: %v", *addr, err)
	}
	reaper.Start(context.Background(), pool, "tech_occupation", "technician")
	srv := grpc.NewServer()
	techv1.RegisterTechnicianServiceServer(srv, technician.NewServer(pool))
	log.Printf("technician: gRPC listening on %s", *addr)
	if err := srv.Serve(lis); err != nil {
		log.Fatalf("technician: serve: %v", err)
	}
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
