-- Technician seed (compose demo, Phase 3): one active technician at the North
-- Workshop fixture dealership (Catalog 002_seed.sql), Europe/London, skilled
-- for the oil-change fixture service (30 min), Mon–Fri shifts 08:00–17:00
-- local minutes. No occupation rows — occupancy is produced by the write path.

INSERT INTO technician (id, dealership_id, active, timezone) VALUES
    ('aaaaaaa1-0000-4000-8000-000000000001', '11111111-1111-4111-8111-111111111111', true, 'Europe/London');

INSERT INTO technician_skill (technician_id, service_type_id, duration_minutes) VALUES
    ('aaaaaaa1-0000-4000-8000-000000000001', '33333333-3333-4333-8333-333333333333', 30);

INSERT INTO technician_shift (technician_id, weekday, start_minutes, end_minutes) VALUES
    ('aaaaaaa1-0000-4000-8000-000000000001', 1, 480, 1020),
    ('aaaaaaa1-0000-4000-8000-000000000001', 2, 480, 1020),
    ('aaaaaaa1-0000-4000-8000-000000000001', 3, 480, 1020),
    ('aaaaaaa1-0000-4000-8000-000000000001', 4, 480, 1020),
    ('aaaaaaa1-0000-4000-8000-000000000001', 5, 480, 1020);