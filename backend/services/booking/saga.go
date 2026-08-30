// Saga: the fixed reserve order (Catalog → Technician → Bay → row) with
// compensation in reverse, and promote-in-place confirm (write-path 4–7).
package booking

import (
	"context"
	"errors"
	"log"
	"time"

	bayv1 "github.com/athandoan/unified-service-scheduler/shared/gen/proto/bay/v1"
	techv1 "github.com/athandoan/unified-service-scheduler/shared/gen/proto/technician/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Allocators are the two gRPC clients (Booking → Technician / Bay only).
type Allocators struct {
	Tech techv1.TechnicianServiceClient
	Bay  bayv1.BayServiceClient
}

// sagaResult carries both occupation ids and the reserved interval.
type sagaResult struct {
	TechOccupationID string
	BayOccupationID  string
	TechnicianID     string
	ServiceBayID     string
	End              time.Time
}

// grpcContext attaches X-Request-Id as gRPC metadata (tracing only — the
// pick-hash salt travels in the message).
func grpcContext(ctx context.Context, requestID string) context.Context {
	return metadata.AppendToOutgoingContext(ctx, "x-request-id", requestID)
}

// latestEnd computes the weekday's shop close in the dealership TZ (step 2):
// hours ceiling, never the occupancy end.
func latestEnd(snap Snapshot, start time.Time) (time.Time, bool) {
	loc, err := time.LoadLocation(snap.Timezone)
	if err != nil {
		return time.Time{}, false
	}
	local := start.In(loc)
	isoWeekday := int(local.Weekday())
	if isoWeekday == 0 {
		isoWeekday = 7
	}
	hours, ok := snap.OpeningHours[isoWeekday]
	if !ok {
		return time.Time{}, false // missing weekday = closed
	}
	closeAt := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, loc).
		Add(time.Duration(hours.CloseMinutes) * time.Minute)
	return closeAt, true
}

// withinHours rejects a start outside snapshotted hours (step 2).
func withinHours(snap Snapshot, start time.Time) bool {
	loc, err := time.LoadLocation(snap.Timezone)
	if err != nil {
		return false
	}
	local := start.In(loc)
	isoWeekday := int(local.Weekday())
	if isoWeekday == 0 {
		isoWeekday = 7
	}
	h, ok := snap.OpeningHours[isoWeekday]
	if !ok {
		return false
	}
	openAt := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, loc).
		Add(time.Duration(h.OpenMinutes) * time.Minute)
	return !start.Before(openAt)
}

// grpcUnavailable maps allocator errors to the 409 path. Per the Phase 2
// contract every allocator Reserve failure is codes.FailedPrecondition.
func grpcUnavailable(err error) bool {
	if err == nil {
		return false
	}
	st, ok := status.FromError(err)
	return ok && st.Code() == codes.FailedPrecondition
}

// reserveSaga runs steps 4–5: Technician Reserve → Bay Reserve. Compensation
// in reverse: bay then tech.
func (s *Service) reserveSaga(ctx context.Context, requestID, serviceTypeID string, snap Snapshot, start time.Time) (sagaResult, error) {
	var res sagaResult
	closeAt, ok := latestEnd(snap, start)
	if !ok {
		return res, ErrInvalid
	}

	gctx := grpcContext(ctx, requestID)

	// Step 4: Technician Reserve (skill-derived end; failure → 409, no bay).
	techRes, err := s.alloc.Tech.Reserve(gctx, &techv1.ReserveRequest{
		DealershipId:  snap.DealershipID,
		ServiceTypeId: serviceTypeID,
		Start:         timestamppb.New(start),
		LatestEnd:     timestamppb.New(closeAt),
		RequestId:     requestID,
	})
	if err != nil {
		return res, ErrUnavailable // no bay call on tech failure
	}
	res.TechOccupationID = techRes.GetOccupationId()
	res.TechnicianID = techRes.GetTechnicianId()
	res.End = techRes.GetEnd().AsTime()

	// Step 5: Bay Reserve with the returned [start, end); failure → release tech.
	bayRes, err := s.alloc.Bay.Reserve(gctx, &bayv1.ReserveRequest{
		DealershipId: snap.DealershipID,
		Start:        timestamppb.New(start),
		End:          techRes.GetEnd(),
		RequestId:    requestID,
	})
	if err != nil {
		s.releaseTech(ctx, res.TechOccupationID, requestID)
		return res, ErrUnavailable
	}
	res.BayOccupationID = bayRes.GetOccupationId()
	res.ServiceBayID = bayRes.GetServiceBayId()
	return res, nil
}

// releaseTech / releaseBay are best-effort compensation: log and return the
// error so callers that must gate on it (the sweeper must not clear a row's
// occupation ids before Release succeeded) can check it. In-request
// compensation may ignore the returned error as before.
func (s *Service) releaseTech(ctx context.Context, occID, requestID string) error {
	if occID == "" {
		return nil
	}
	_, err := s.alloc.Tech.Release(grpcContext(ctx, requestID),
		&techv1.ReleaseRequest{OccupationId: occID})
	if err != nil {
		log.Printf("booking: release tech %s failed: %v (TTL 120s bounds the pin)", occID, err)
	}
	return err
}

func (s *Service) releaseBay(ctx context.Context, occID, requestID string) error {
	if occID == "" {
		return nil
	}
	_, err := s.alloc.Bay.Release(grpcContext(ctx, requestID),
		&bayv1.ReleaseRequest{OccupationId: occID})
	if err != nil {
		log.Printf("booking: release bay %s failed: %v (TTL 120s bounds the pin)", occID, err)
	}
	return err
}

// releaseBoth compensates in reverse saga order: bay first, then tech.
func (s *Service) releaseBoth(ctx context.Context, bayOccID, techOccID, requestID string) {
	s.releaseBay(ctx, bayOccID, requestID)
	s.releaseTech(ctx, techOccID, requestID)
}

// confirmBoth promotes HELD → CONFIRMED in place (step 7, promote-in-place:
// never a second claim). Any failure → release both, never a replacement pair.
func (s *Service) confirmBoth(ctx context.Context, techOccID, bayOccID, requestID string) error {
	gctx := grpcContext(ctx, requestID)
	if _, err := s.alloc.Tech.Confirm(gctx, &techv1.ConfirmRequest{OccupationId: techOccID}); err != nil {
		return err
	}
	if _, err := s.alloc.Bay.Confirm(gctx, &bayv1.ConfirmRequest{OccupationId: bayOccID}); err != nil {
		return err
	}
	return nil
}

var _ = errors.Is // errors kept for mapping expansions
