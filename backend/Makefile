.PHONY: compose-up compose-down migrate run-catalog run-technician run-bay run-booking run-gateway build test test-unit test-e2e test-load protos image-build image-push k8s-apply k8s-delete

# ---- k8s deploy (Helm chart at ../deploy/k8s) ----
IMAGE_PREFIX ?= scheduler
IMAGE_TAG ?= latest
# Cluster nodes are Ampere (arm64) — build images for that platform.
PLATFORM ?= linux/arm64
HELM_RELEASE ?= scheduler
HELM_NAMESPACE ?= scheduler
CHART := ../deploy/k8s

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

# Build the six scheduler images via the multi-stage Dockerfile's PKG arg.
# postgres:16 is pulled upstream, never built.
image-build:
	docker build --platform $(PLATFORM) --build-arg PKG=./shared/migrate           -t $(IMAGE_PREFIX)/migrate:$(IMAGE_TAG) .
	docker build --platform $(PLATFORM) --build-arg PKG=./services/catalog/cmd     -t $(IMAGE_PREFIX)/catalog:$(IMAGE_TAG) .
	docker build --platform $(PLATFORM) --build-arg PKG=./services/technician/cmd  -t $(IMAGE_PREFIX)/technician:$(IMAGE_TAG) .
	docker build --platform $(PLATFORM) --build-arg PKG=./services/bay/cmd         -t $(IMAGE_PREFIX)/bay:$(IMAGE_TAG) .
	docker build --platform $(PLATFORM) --build-arg PKG=./services/booking/cmd     -t $(IMAGE_PREFIX)/booking:$(IMAGE_TAG) .
	docker build --platform $(PLATFORM) --build-arg PKG=./services/gateway         -t $(IMAGE_PREFIX)/gateway:$(IMAGE_TAG) .
	# Harness: static nginx stub (no build step).
	docker build --platform $(PLATFORM) -f ../harness/Dockerfile -t $(IMAGE_PREFIX)/harness:$(IMAGE_TAG) ../harness

image-push:
	docker push $(IMAGE_PREFIX)/migrate:$(IMAGE_TAG)
	docker push $(IMAGE_PREFIX)/catalog:$(IMAGE_TAG)
	docker push $(IMAGE_PREFIX)/technician:$(IMAGE_TAG)
	docker push $(IMAGE_PREFIX)/bay:$(IMAGE_TAG)
	docker push $(IMAGE_PREFIX)/booking:$(IMAGE_TAG)
	docker push $(IMAGE_PREFIX)/gateway:$(IMAGE_TAG)
	docker push $(IMAGE_PREFIX)/harness:$(IMAGE_TAG)

# Deploy flow:
#  1. Jobs are immutable -> delete the previous migrate Job before upgrading.
#  2. helm upgrade --install rolls out the stack (migrate is a normal Job,
#     NOT a Helm hook, so `helm --wait` is not a migration gate).
#  3. Wait for postgres Ready, THEN wait for the migrate Job to complete.
k8s-apply:
	kubectl delete job -n $(HELM_NAMESPACE) -l app.kubernetes.io/component=migrate --ignore-not-found
	helm upgrade --install $(HELM_RELEASE) $(CHART) -n $(HELM_NAMESPACE) --create-namespace \
	  --set imagePrefix=$(IMAGE_PREFIX) --set imageTag=$(IMAGE_TAG)
	kubectl rollout status deployment/$(HELM_RELEASE)-postgres -n $(HELM_NAMESPACE) --timeout=120s
	kubectl wait --for=condition=complete job/$(HELM_RELEASE)-migrate -n $(HELM_NAMESPACE) --timeout=180s

k8s-delete:
	helm uninstall $(HELM_RELEASE) -n $(HELM_NAMESPACE)