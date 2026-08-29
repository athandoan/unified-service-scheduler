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

## Test

## Deploy