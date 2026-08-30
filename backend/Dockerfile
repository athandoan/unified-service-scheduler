# One multi-stage Dockerfile builds any scheduler binary via the PKG build arg,
# e.g. ./services/catalog/cmd, ./services/gateway, ./shared/migrate. Migrate
# builds also carry the SQL migrations tree laid out as
# /migrations/<root>/services/<db>/migrations, which is the layout the migrate
# tool expects (-root /migrations).
#
# Runtime is alpine (no shell-dependent HEALTHCHECK; compose gates startup by
# retrying curl against the gateway from the host). Services bind 0.0.0.0 so
# the compose network can reach them.
#
# Build examples:
#   docker build --build-arg PKG=./shared/migrate           -t scheduler-migrate .
#   docker build --build-arg PKG=./services/catalog/cmd     -t scheduler-catalog .
#   docker build --build-arg PKG=./services/gateway         -t scheduler-gateway .

# ---- build stage ----
FROM golang:1.26-alpine AS build
ARG PKG=./services/catalog/cmd
ENV CGO_ENABLED=0
WORKDIR /src

# Cache module downloads separately from source.
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN go build -trimpath -ldflags="-s -w" -o /out/app "$PKG" && \
    if [ "$PKG" = "./shared/migrate" ]; then \
      for db in catalog technician bay booking; do \
        mkdir -p "/out/migrations/services/$db/migrations" && \
        cp "services/$db"/migrations/*.sql "/out/migrations/services/$db/migrations/"; \
      done; \
    fi

# ---- runtime stage ----
FROM alpine:3.20
# ca-certificates: outbound http(s) from booking (catalog client) and gateway.
RUN apk add --no-cache ca-certificates tzdata && adduser -D -u 10001 app
COPY --from=build /out/ /
USER app
ENTRYPOINT ["/app"]