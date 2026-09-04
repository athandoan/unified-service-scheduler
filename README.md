# Unified Service Scheduler

Keyloop technical assessment, **Scenario A (Ownership)**.

The architectural plan is [`docs/SYSTEM_DESIGN.md`](./docs/SYSTEM_DESIGN.md). Implementation is in [`backend/`](./backend/).

## Plan

- [x] **System design** — done: [`docs/SYSTEM_DESIGN.md`](./docs/SYSTEM_DESIGN.md).
- [x] **Implement** — write path: **Technician** + **Bay** + **Booking** (Go + pgx). Each allocator has PostgreSQL GiST on its resource. Booking orchestrates and compensates. Catalog and the client stay **stubs** (OpenAPI + harness). Gateway is a REST edge, **not** the saga coordinator.
- [x] **Test** — concurrent overlap on the **same tech** and the **same bay**; plus tech reserved / bay fails → tech released. Happy-path coverage alone is not enough.
- [x] **Deploy** — local first (compose above). Kubernetes is a Helm chart at [`deploy/k8s/`](./deploy/k8s/), not the occupancy problem. Extra pods do not add occupancy scale.

Assessment leftovers: the video, if still required. The AI Collaboration Narrative is below and stays as the design-session account.

Go, Docker, protos, and migrations live in [`backend/`](./backend/). Docs, OpenAPI, and the client harness stay at the repo root.

## Local run

No Go toolchain needed to run the app — Docker Compose runs Postgres 16 and every service, including the one-shot migration job:

```bash
cd backend
docker compose up --build -d
```

Then retry until the Gateway answers (first start builds all images):

```bash
curl -sS http://localhost:8080/dealerships
```

`curl -sS http://localhost:8080/dealerships` returns a JSON array including North Workshop `11111111-1111-4111-8111-111111111111`. Booking reads and all writes go through the Gateway on :8080. Once compose is up, the seeded North Workshop dealership can be booked end-to-end through the Gateway (seeded technician and bay are migrations, not test fixtures).

Ports: Gateway **:8080** (the only published app port), Catalog **:8081**, Booking **:8082**, Technician gRPC **9091**, Bay gRPC **9092** (internal to the compose network).

Contributor-only (requires Go and the published Postgres on :5432 from the compose above):

```bash
cd backend && go test ./... -count=1
```

`go test -short ./...` runs the mocked unit tests only (no Postgres needed). It is **not** the occupancy gate — the unmocked overlap/compensate tests above are.

### End-to-end (compose Gateway)

Black-box E2E over the **public API only**: every HTTP call goes to the Gateway on :8080, seeding/cleanup uses the compose Postgres on :5432. It never touches :8081/:8082 or gRPC :9091/:9092. The compose demo seed (migrations) seeds a technician and bay onto North Workshop for the live harness; the E2E suite does not use those — it never seeds technicians/bays onto the fixture dealer, and each write case owns and cleans up its own test dealer/tech/bay rows.

After `docker compose up --build -d`:

```bash
cd backend && make test-e2e
```

which runs `go test -tags e2e ./e2e/... -count=1` (the `e2e` build tag keeps the suite out of `go test ./...` and `go test -short ./...`). The Gateway must already be up — the suite fails, not skips, when it is not. `GATEWAY_URL` overrides `http://localhost:8080`.

Load tests: **pending** — `make test-load` is a stub.

### Kubernetes (Helm)

Compose remains the local loop. The cluster path is one chart, **replicas: 1**. Services stay ClusterIP; two public hostnames on Gateway API HTTPRoute: BE `api.rjx.dedyn.io` → `svc/gateway:8080`, FE `scheduler.rjx.dedyn.io` → the harness stub (nginx, still a stub — not a product UI).

```bash
cd backend
make image-build image-push IMAGE_PREFIX=<registry>/scheduler
make k8s-apply IMAGE_PREFIX=<registry>/scheduler
curl -sS https://api.rjx.dedyn.io/dealerships
```

Override the hosts with `--set httpRoute.apiHost=... --set httpRoute.appHost=...` or disable both with `--set httpRoute.enabled=false`. Port-forward still works as a fallback: `kubectl -n scheduler port-forward svc/gateway 8080:8080`.

`IMAGE_PREFIX` must be a registry the **nodes** can pull (not laptop `127.0.0.1:5000`). `PLATFORM` defaults to `linux/arm64` (Ampere). `postgres:16` is upstream.

If the registry needs auth, create a `kubernetes.io/dockerconfigjson` secret in namespace `scheduler` (do not commit it) and pass its name:

```bash
helm upgrade --install scheduler ../deploy/k8s -n scheduler --create-namespace \
  --set imagePrefix=<registry>/scheduler --set imageTag=latest \
  --set 'imagePullSecrets[0].name=registry'
```

`make k8s-apply` waits for postgres Ready, then for the migrate **Job** to complete (`helm --wait` is not that gate). `make k8s-delete` uninstalls the release.

## Client stub

Static HTML/JS harness for Catalog reads and Booking’s public API — no slot grid. Not a product UI. Not the frontend track. Mock mode is **not** the overlap test. Never `/occupations`.

Open [`harness/index.html`](./harness/index.html) in a browser, or serve the folder:

```bash
python3 -m http.server 8000 --directory harness
```

then visit `http://localhost:8000`. Toggle **mock** (no Go backend) vs **localhost**. Contracts: [`openapi/`](./openapi/).

---

## AI Collaboration Narrative

This section is the design-session account. GenAI did not write production code in the design phase; implementation in [`backend/`](./backend/) and per-phase authorship are in [`docs/AI_LOG.md`](./docs/AI_LOG.md) (Implement / Test / Deploy).

**Guiding.** A confirmed job needs a free bay and a free qualified technician for the whole interval; silent double-book is the failure that matters. Duration is not taken from the client. Occupancy is not Redis. Direction was those constraints, not “design a scheduler.”

**Verifying.** Human review changed the design three times: Booking-only was not in the brief (backend or frontend track); the user chose two occupancy services instead of one Booking database; “real-time” is the hold/confirm write, not the slot grid.

**Quality.** The author kept the occupancy invariant when the model preferred a more distributed story. Named holes stay visible (IDOR on get/cancel, Catalog TOCTOU, same-VIN overlap, non-idempotent holds, a crash can pin a hold until TTL).
