package booking_test

import (
	"context"
	"github.com/athandoan/unified-service-scheduler/shared/pkg/integrationtest"
	"google.golang.org/grpc"
	"google.golang.org/grpc/test/bufconn"
	"net"
	"testing"
	"time"
)

// bufconn wiring for in-process allocator gRPC servers (real DBs, no ports).
func bufconnListen() *bufconn.Listener {
	return bufconn.Listen(1 << 20)
}
func bufconnDialer(lis *bufconn.Listener) func(context.Context, string) (net.Conn, error) {
	return func(ctx context.Context, _ string) (net.Conn, error) {
		return lis.DialContext(ctx)
	}
}

var _ = grpc.DialContext // silence unused if API changes
// testZone is re-exported for the tests below.
func testZone(t *testing.T) *time.Location { return integrationtest.TestZone(t) }
