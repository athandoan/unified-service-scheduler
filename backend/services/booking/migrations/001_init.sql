-- Booking: appointment record. Opaque ids everywhere; no GiST, no occupancy SQL.

CREATE TABLE appointment (
    id                  uuid PRIMARY KEY,
    dealership_id       uuid NOT NULL,
    customer_id         uuid NOT NULL,
    vehicle_id          uuid NOT NULL,
    service_type_id     uuid NOT NULL,
    technician_id       uuid,
    service_bay_id      uuid,
    tech_occupation_id  uuid,
    bay_occupation_id   uuid,
    start_at            timestamptz NOT NULL,
    end_at              timestamptz NOT NULL CHECK (end_at > start_at),
    status              text NOT NULL CHECK (status IN ('HELD', 'CONFIRMED', 'CANCELLED')),
    hold_expires_at     timestamptz,
    idempotency_key     text,
    request_fingerprint text
);

-- Idempotency on CONFIRMED only; holds do not honour the key.
CREATE UNIQUE INDEX appointment_confirmed_idempotency_idx ON appointment (idempotency_key)
    WHERE (idempotency_key IS NOT NULL AND status = 'CONFIRMED');

CREATE INDEX appointment_dealership_idx ON appointment (dealership_id);
CREATE INDEX appointment_status_idx ON appointment (status);
CREATE INDEX appointment_hold_expiry_idx ON appointment (hold_expires_at)
    WHERE status = 'HELD';