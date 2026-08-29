# Unified Service Scheduler

Keyloop technical assessment, **Scenario A (Ownership)**.

The architectural plan is [`docs/SYSTEM_DESIGN.md`](./docs/SYSTEM_DESIGN.md). There is no Booking service and no `go test` or start command **yet** — the brief's design-vs-working-code split is the assessment shape; this repo's first step is design, and the implement / test / deploy plan is below.

## Plan

- [x] **System design** — done: [`docs/SYSTEM_DESIGN.md`](./docs/SYSTEM_DESIGN.md).
- [ ] **Implement** — write path: **Technician** + **Bay** + **Booking** (Go + pgx). Each allocator has PostgreSQL GiST on its resource. Booking orchestrates and compensates. Catalog and the client stay **stubs** (OpenAPI + harness). Gateway is a REST edge, **not** the saga coordinator.
- [ ] **Test** — concurrent overlap on the **same tech** and the **same bay**; plus tech reserved / bay fails → tech released. Happy-path coverage alone is not enough. No fake tests today; tests land with the code.
- [ ] **Deploy** — later: the three write services + their Postgres, local first. Kubernetes/Jenkins is not the problem. Extra pods do not add occupancy scale.

Assessment leftovers: the video, if still required. The AI Collaboration Narrative is below and stays as the design-session account.

## Client stub

Static HTML/JS harness for Catalog reads and Booking’s public API — no slot grid. Not a product UI. Not the frontend track. Mock mode is **not** the overlap test. Never `/occupations`.

Open [`harness/index.html`](./harness/index.html) in a browser, or serve the folder:

```bash
python3 -m http.server 8000 --directory harness
```

then visit `http://localhost:8000`. Toggle **mock** (no Go backend) vs **localhost**. Contracts: [`openapi/`](./openapi/).

---

## AI Collaboration Narrative

This section is the design-session account, not an implementation. GenAI did not write production code. What was reviewed and what changed in the architecture is in [`docs/AI_LOG.md`](./docs/AI_LOG.md) under **System design**.

**Guiding.** A confirmed job needs a free bay and a free qualified technician for the whole interval; silent double-book is the failure that matters. Duration is not taken from the client. Occupancy is not Redis. Direction was those constraints, not “design a scheduler.”

**Verifying.** Human review changed the design three times: Booking-only was not in the brief (backend or frontend track); the user chose two occupancy services instead of one Booking database; “real-time” is the hold/confirm write, not the slot grid.

**Quality.** The author kept the occupancy invariant when the model preferred a more distributed story. Named holes stay visible (IDOR on get/cancel, Catalog TOCTOU, same-VIN overlap, non-idempotent holds, a crash can pin a hold until TTL). Final quality of **code** is not claimed — there is no production code yet.
