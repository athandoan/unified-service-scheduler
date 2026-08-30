-- Technician allocator: technicians, skills, shifts, absences, tech occupations.
-- Catalog UUIDs are opaque here (no cross-DB FKs).

CREATE EXTENSION IF NOT EXISTS btree_gist;

CREATE TABLE technician (
    id            uuid PRIMARY KEY,
    dealership_id uuid NOT NULL,
    active        boolean NOT NULL DEFAULT true
);

CREATE TABLE technician_skill (
    technician_id     uuid NOT NULL REFERENCES technician (id) ON DELETE CASCADE,
    service_type_id   uuid NOT NULL,
    duration_minutes  int  NOT NULL CHECK (duration_minutes >= 1),
    PRIMARY KEY (technician_id, service_type_id)
);

CREATE TABLE technician_shift (
    technician_id uuid NOT NULL REFERENCES technician (id) ON DELETE CASCADE,
    weekday       smallint NOT NULL CHECK (weekday BETWEEN 1 AND 7),
    start_minutes int NOT NULL CHECK (start_minutes >= 0 AND start_minutes < 1440),
    end_minutes   int NOT NULL CHECK (end_minutes > 0 AND end_minutes <= 1440),
    PRIMARY KEY (technician_id, weekday)
);

CREATE TABLE technician_absence (
    id            uuid PRIMARY KEY,
    technician_id uuid NOT NULL REFERENCES technician (id) ON DELETE CASCADE,
    start_at      timestamptz NOT NULL,
    end_at        timestamptz NOT NULL CHECK (end_at > start_at)
);

CREATE TABLE tech_occupation (
    id              uuid PRIMARY KEY,
    technician_id   uuid NOT NULL REFERENCES technician (id) ON DELETE CASCADE,
    start_at        timestamptz NOT NULL,
    end_at          timestamptz NOT NULL CHECK (end_at > start_at),
    status          text NOT NULL CHECK (status IN ('HELD', 'CONFIRMED', 'RELEASED')),
    hold_expires_at timestamptz,
    appointment_id  uuid
);

-- One claim authority per resource: a technician cannot hold two live
-- (HELD|CONFIRMED) intervals that overlap. RELEASED rows drop out.
ALTER TABLE tech_occupation ADD CONSTRAINT tech_occupation_no_overlap
EXCLUDE USING gist (
    technician_id WITH =,
    tstzrange(start_at, end_at, '[)') WITH &&
) WHERE (status IN ('HELD', 'CONFIRMED'));

CREATE INDEX tech_occupation_technician_idx ON tech_occupation (technician_id);
CREATE INDEX tech_occupation_expiry_idx ON tech_occupation (hold_expires_at)
    WHERE status = 'HELD';