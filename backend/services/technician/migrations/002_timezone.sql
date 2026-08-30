-- Technician timezone: local-minute shifts need the tech's IANA zone at claim
-- time. Denormalized attribute (not a Catalog FK); allocators never call Catalog.
ALTER TABLE technician ADD COLUMN timezone text NOT NULL DEFAULT 'UTC';
ALTER TABLE technician ALTER COLUMN timezone DROP DEFAULT;