// Command booking serves the public Booking HTTP API on :8082 (:8080 is the
// Gateway). Own Postgres: booking DB only. Catalog via HTTP (fail-closed);
// allocators via gRPC.
package main

import (
	"context"
	"flag"
	"log"
	"net/http"
	"os"

	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/athandoan/unified-service-scheduler/services/booking"
	bayv1 "github.com/athandoan/unified-service-scheduler/shared/gen/proto/bay/v1"
	techv1 "github.com/athandoan/unified-service-scheduler/shared/gen/proto/technician/v1"
)

func main() {
	addr := flag.String("addr", envOr("BOOKING_ADDR", ":8082"),
		"listen address (never :8080 — that is the Gateway)")
	dsn := flag.String("dsn", envOr("BOOKING_DATABASE_URL",
		"postgres://scheduler:scheduler@localhost:5432/booking?sslmode=disable"), "booking Postgres DSN")
	catalogURL := flag.String("catalog-url", envOr("BOOKING_CATALOG_URL", "http://localhost:8081"), "Catalog base URL")
	techAddr := flag.String("tech-addr", envOr("BOOKING_TECHNICIAN_ADDR", "localhost:9091"), "Technician gRPC address")
	bayAddr := flag.String("bay-addr", envOr("BOOKING_BAY_ADDR", "localhost:9092"), "Bay gRPC address")
	flag.Parse()

	pool, err := pgxpool.New(context.Background(), *dsn)
	if err != nil {
		log.Fatalf("booking: connect: %v", err)
	}
	if err := pool.Ping(context.Background()); err != nil {
		log.Fatalf("booking: ping: %v", err)
	}

	techConn, err := grpc.NewClient(*techAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		log.Fatalf("booking: technician client: %v", err)
	}
	bayConn, err := grpc.NewClient(*bayAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		log.Fatalf("booking: bay client: %v", err)
	}

	svc := booking.NewService(
		booking.NewStore(pool),
		booking.NewCatalogClient(*catalogURL),
		booking.Allocators{
			Tech: techv1.NewTechnicianServiceClient(techConn),
			Bay:  bayv1.NewBayServiceClient(bayConn),
		},
	)
	svc.StartSweeper(context.Background())

	log.Printf("booking: listening on %s (catalog=%s tech=%s bay=%s)", *addr, *catalogURL, *techAddr, *bayAddr)
	if err := http.ListenAndServe(*addr, booking.NewHandler(svc)); err != nil {
		log.Fatalf("booking: serve: %v", err)
	}
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
