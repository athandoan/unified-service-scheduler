-- Bay allocator: service bays and bay occupations.
-- Bays are homogeneous: no capability, no tech×bay. No seed rows.

CREATE EXTENSION IF NOT EXISTS btree_gist;

CREATE TABLE service_bay (
    id            uuid PRIMARY KEY,
    dealership_id uuid NOT NULL,
    active        boolean NOT NULL DEFAULT true
);

CREATE TABLE bay_occupation (
    id              uuid PRIMARY KEY,
    service_bay_id  uuid NOT NULL REFERENCES service_bay (id) ON DELETE CASCADE,
    start_at        timestamptz NOT NULL,
    end_at          timestamptz NOT NULL CHECK (end_at > start_at),
    status          text NOT NULL CHECK (status IN ('HELD', 'CONFIRMED', 'RELEASED')),
    hold_expires_at timestamptz,
    appointment_id  uuid
);

ALTER TABLE bay_occupation ADD CONSTRAINT bay_occupation_no_overlap
EXCLUDE USING gist (
    service_bay_id WITH =,
    tstzrange(start_at, end_at, '[)') WITH &&
) WHERE (status IN ('HELD', 'CONFIRMED'));

CREATE INDEX bay_occupation_bay_idx ON bay_occupation (service_bay_id);
CREATE INDEX bay_occupation_expiry_idx ON bay_occupation (hold_expires_at)
    WHERE status = 'HELD';