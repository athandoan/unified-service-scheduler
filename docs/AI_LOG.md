# AI log

Collaboration record for System design / Implement / Test / Deploy — not a source of truth; live architecture stays in `docs/SYSTEM_DESIGN.md`.

## System design

- Booking-only
  - **AI:** treated Booking-only as the brief.
  - **Reviewed:** brief is backend or frontend track.
  - **Changed:** local cut, not a Keyloop sentence.

- Occupancy split
  - **AI:** first design had occupancy on one Booking DB; the model did not invent GiST; it offered Redis occupancy, Kafka, and the slot list as a lock.
  - **Reviewed:** user chose Technician Time + Bay.
  - **Changed:** two occupancy services, Booking coordinates (saga); a crash can pin a hold until TTL.

- Real-time
  - **AI:** treated the slot grid as the check.
  - **Reviewed:** user locked the hold/confirm write (tech + bay reserve).
  - **Changed:** not the grid, not Booking’s DB.

- Diagram cleanup
  - **AI:** diagram had ingress arrows to Technician Time and Bay (looked public).
  - **Reviewed:** user asked diagram cleanup (edges + stub vs live + un-nest).
  - **Changed:** clients → Catalog/Booking only; Technician Time and Bay stay internal (Booking reserve/release). Extra gateway rejected.

- Slots service
  - **AI:** advisory Slots service.
  - **Reviewed:** user didn’t need that split.
  - **Changed:** dropped; public Catalog + Booking; allocators write-only.

- Capacity labels
  - **AI:** 20k dealers × 50 bookings/day as the write-QPS sketch.
  - **Reviewed:** user asked if that matches reality and 3–5 years.
  - **Changed:** QPS numbers stay (O(10²) peak); 20k is Keyloop-global OOM, not US-only; 50/day is bay-capped high-mix stress, not today’s mix.

- Gateway + gRPC internals
  - **AI:** pass-through Ingress + HTTP saga hops; occupations listed as internal HTTP.
  - **Reviewed:** user wants gRPC internals + Gateway service.
  - **Changed:** Gateway = REST edge (not aggregator/saga); allocator hops = gRPC Reserve/Confirm/Release; Catalog snapshot stays public HTTP; occupations off the public sketch.

- Rename Technician Time → Technician
  - **AI:** named the interval allocator Technician Time.
  - **Reviewed:** user wants Technician.
  - **Changed:** renamed the service only (not the person table).

- Service-type qualification
  - **AI:** implicit skill filter in Reserve
  - **Reviewed:** user wants service type + qualification explicit in model/API/gRPC
  - **Changed:** binary tech × `service_type`; gRPC messages in the design sketch

- Skill duration as occupancy
  - **AI:** catalogue `duration_minutes` as occupancy length; skill binary filter only.
  - **Reviewed:** user wants occupancy `end` from `TechnicianSkill.duration_minutes`; catalog duration display-only.
  - **Changed:** tech claim computes `end` in INSERT…SELECT; `latest_end` hours ceiling; Bay uses returned interval; named residual: hash-pick then Bay `409` can miss a shorter tech.

- Unavailability shapes
  - **AI:** a table per reason; hours as a filter in the tech SQL.
  - **Reviewed:** user wants claim vs predicate documented; no table per label; hours stay Catalog.
  - **Changed:** two shapes in design; absences reused for dated tech time-off; lunch stays shift; holidays remain out of scope; no reason column.

- Tighten design doc
  - **AI:** doc carried long repeated locks (skill duration, Redis/slots bans, Gateway not-JWT/BFF restated) and had no sequence diagram.
  - **Reviewed:** user wants less boilerplate and one sequenceDiagram; residuals must stay visible, not name-only; write-path 1–7 and assumptions list stay readable.
  - **Changed:** cut repetition (Gateway clone paragraph, duplicate skill-duration clauses, Redis/slots/capacity caveats to once); added one reserve sequenceDiagram under Data flow; write-path 1–7 kept; architecture unchanged.

- Saga recovery
  - **AI:** crash pin was stated as until-TTL only; Confirm/Release treated missing as no-op both ways; confirm-without-hold inserted `CONFIRMED` after reserve.
  - **Reviewed:** user wanted die-mid-saga / TTL / sweeper-vs-outbox documented.
  - **Changed:** Confirm missing/expired/`RELEASED` fails; Release missing/`RELEASED` no-op (`CONFIRMED` stays releasable); any Confirm fail Releases both; `POST /holds` returns after `HELD` insert (must not Confirm); confirm-without-hold persists `HELD` then Confirm then `UPDATE CONFIRMED`; sweeper CAS then Release, no outbox; GET after TTL is `CANCELLED`; residual pin only before Booking row; periodic allocator reaper.

- Data model layout
  - **AI:** all four DBs described then all four ER diagrams; TechnicianSkill bullet was a write-path essay (INSERT…SELECT, 409, gRPC).
  - **Reviewed:** per-database prose then that ER; skill is presence + occupancy duration, not the write path.
  - **Changed:** interleaved prose+ER per database; prose is invariants only; skill bullet cut to presence + occupancy duration; write-path left in steps 1–7.

- Data flow clarity
  - **AI:** one numbered 1–7 list for three HTTP journeys; reserve diagram read as the whole flow; reaper sat after step 7.
  - **Reviewed:** three journeys — `/holds` (1–2, 4–6, no Confirm), confirm-without-hold (1–6 then 7 same request), promote (`holdId`, 3 then 7, second request).
  - **Changed:** 3-row entry map; diagram caption + insert `HELD`; split Reserve 1–6 / Confirm 7; reaper moved into Recovery; no extra diagram.

## Implement

- OpenAPI/curl/harness
  - **AI:** OpenAPI/curl/harness as the brief’s client stand-in.
  - **Reviewed:** harness for testing; backend still the assessment.
  - **Changed:** stub not frontend track; never `/occupations`; not Go.

- Backend Phase 1–2 (foundation + allocators)
  - **AI:** built compose (Postgres 16, four DBs), four SQL migrations with a pgx runner, Catalog stub on :8081, two unary protos (Technician/Bay), and the allocators’ INSERT…SELECT claims on :9091/:9092.
  - **Reviewed:** user locked runtime (Go on host, Docker = Postgres only, :8080 = Gateway), Reserve message shapes, TTL 120s in SQL, request_id as pick-hash salt.
  - **Changed:** btree_gist EXCLUDE per allocator; Bay claim has no predicates (GiST sole authority); technician.timezone denormalized for local-minute shifts; 23P01 → delete expired HELD → retry ×3.

- Backend Phase 3 (Booking + Gateway)
  - **AI:** Booking saga (Catalog snapshot fail-closed → Reserve tech → Reserve bay → INSERT HELD → Confirm tech then bay), Gateway as a thin reverse-proxy on :8080, Booking sweeper + allocator reapers.
  - **Reviewed:** user confirmed write-path 1–7, confirm-only idempotency, POST /holds returns HELD and must not Confirm, compensate bay-then-tech.
  - **Changed:** Booking owns only the appointment row (no occupancy SQL); idempotency_key/fingerprint land only on the CONFIRMED UPDATE; any Confirm failure releases both and 409s; never a replacement pair.

## Test

- Occupancy tests
  - **AI:** first pass used claim predicates that made GiST removable without failing tests.
  - **Reviewed:** user rule — tests must be able to break the occupancy invariant.
  - **Changed:** Bay claim reduced to pick-only, with skip-busy `NOT EXISTS` plus the GiST exclusion as the race authority; the same-bay public test would be defeated by dropping Bay GiST; same-tech public test runs with ≥2 bays so Bay cannot mask a dropped technician GiST; same-tech and compensate read live allocator DBs.

- Public tests (three)
  - **AI:** three public HTTP tests through Booking (in-process): same-tech overlap (1 tech ≥2 bays), same-bay overlap (≥2 techs 1 bay), compensate (bay fail → tech RELEASED).
  - **Reviewed:** topologies from the lock table; concurrent (goroutines, gate), test-owned rows, no migration seed.
  - **Changed:** all three pass against compose Postgres; loser gets 409; compensate verified by reading the technician DB.

- Mocked unit tests
  - **AI:** occupancy tests as the only suite (real Postgres).
  - **Reviewed:** user wants unit / E2E / load split; mocked units first for CI without Postgres; occupancy must still break GiST.
  - **Changed:** `go test -short` skips occupancy via `Connect()` only; Booking/Gateway httptest fakes; `make test` occupancy command unchanged; `-short` is not the occupancy gate.

- Gateway E2E
  - **AI:** in-process Booking tests as the public API check.
  - **Reviewed:** user wants a local E2E suite against compose Gateway `:8080`; load tests pending.
  - **Changed:** `backend/e2e` `//go:build e2e`; test-owned dealers (not fixture North Workshop); HTTP only `:8080`; Alpine images include `tzdata` so IANA hours work; `make test-e2e`; `make test-load` stub.

## Deploy

- Helm chart + cluster apply
  - **AI:** Kustomize first; then Helm because the machine already has Helm.
  - **Reviewed:** user chose Helm; oracle: migrate is a Job not a hook; `replicas: 1` hardcoded; smoke is port-forward `/dealerships`; arm64 `PLATFORM` and optional `imagePullSecrets` names (no credentials in git).
  - **Changed:** `deploy/k8s/` chart; `make image-build/image-push/k8s-apply/k8s-delete`; live smoke 200 on Gateway; README Deploy ticked.

- Public hostname
  - **AI:** ClusterIP + port-forward only (no Ingress).
  - **Reviewed:** user wants a domain; cluster already uses Gateway API HTTPRoute on `public-gateway` / `*.rjx.dedyn.io`.
  - **Changed:** HTTPRoute `scheduler.rjx.dedyn.io` → `gateway:8080` plus HTTP→HTTPS redirect. Superseded below (never applied; BE moved to `api`).

- FE + BE domains
  - **AI:** one domain for the Gateway; no product frontend in repo.
  - **Reviewed:** user wants one domain each: FE `scheduler.rjx.dedyn.io` serves the harness stub, BE `api.rjx.dedyn.io` serves Gateway; harness stays a stub.
  - **Changed:** 7th `harness` nginx image; HTTPRoutes api → `gateway:8080`, scheduler → `harness:80` (+ redirects); live smoke 200 on both; harness live-mode Base URL defaults to the BE domain when served remotely.