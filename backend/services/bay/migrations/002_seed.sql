-- Bay seed (compose demo, Phase 3): one active service bay at the North
-- Workshop fixture dealership (Catalog 002_seed.sql). Bays are homogeneous —
-- no capability columns. No occupation rows — occupancy is produced by the
-- write path.

INSERT INTO service_bay (id, dealership_id, active) VALUES
    ('aaaaaaa2-0000-4000-8000-000000000002', '11111111-1111-4111-8111-111111111111', true);