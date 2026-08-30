.PHONY: compose-up compose-down migrate run-catalog run-technician run-bay run-booking run-gateway build test test-unit test-e2e test-load protos

compose-up:
	docker compose up --build -d

compose-down:
	docker compose down

migrate:
	go run ./shared/migrate

run-catalog:
	go run ./services/catalog/cmd

run-technician:
	go run ./services/technician/cmd

run-bay:
	go run ./services/bay/cmd

run-booking:
	go run ./services/booking/cmd

run-gateway:
	go run ./services/gateway

build:
	go build ./...

test:
	go test ./...

# Unit tests: -short skips the real-Postgres occupancy tests (no DB needed).
# Not the occupancy gate.
test-unit:
	go test -short ./...

# E2E: black-box suite against the compose Gateway on :8080 (docker compose up
# first). Tagged e2e so `go test ./...` never runs it.
test-e2e:
	go test -tags e2e ./e2e/... -count=1

# Load tests: not implemented (Phase 1 is functional E2E only).
test-load:
	@echo "not implemented"; exit 1

protos:
	buf generate