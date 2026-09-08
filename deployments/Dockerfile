# Multi-stage build producing a ~15 MB image from a distroless base.
#
# Image size is not vanity here: Cloud Run scales to zero, so every request
# after an idle period pays the cold-start cost of pulling and starting this
# image. A 15 MB static binary starts in a few hundred milliseconds where a
# 900 MB image with a full distro does not — and NFR-2 only allows 8 seconds
# end to end, most of which the AI vendor will spend.
#
# Build either binary with --build-arg:
#   docker build -f deployments/Dockerfile --build-arg CMD=api    -t ayna-api .
#   docker build -f deployments/Dockerfile --build-arg CMD=worker -t ayna-worker .

ARG GO_VERSION=1.26

# --- build ---------------------------------------------------------------
FROM golang:${GO_VERSION} AS build

WORKDIR /src

# Dependencies are copied and downloaded before the source so this layer is
# cached across code changes — the slow step only re-runs when go.mod moves.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

ARG CMD=api
ARG VERSION=dev

# CGO_ENABLED=0 makes the binary static, which is what allows the distroless
# base below to have no libc at all.
#   -s -w        strip symbol and DWARF tables (smaller image)
#   -X main.version  stamps the build so /healthz can report which commit it is
RUN CGO_ENABLED=0 GOOS=linux go build \
    -trimpath \
    -ldflags="-s -w -X main.version=${VERSION}" \
    -o /out/app ./cmd/${CMD}

# --- runtime -------------------------------------------------------------
# Distroless: no shell, no package manager, no busybox. If this container is
# ever compromised there is nothing in it to pivot with. `nonroot` drops
# privileges, which Cloud Run expects anyway.
FROM gcr.io/distroless/static-debian12:nonroot

# The Go binary makes outbound HTTPS calls (Auth0, R2, the AI vendor), which
# needs a CA bundle. Distroless static ships one; copying it explicitly keeps
# the dependency visible rather than implicit.
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=build /out/app /app

USER nonroot:nonroot

# Documentation only — Cloud Run injects the real port via $PORT, and the
# config package reads it. A hardcoded port here would be ignored.
EXPOSE 8080

ENTRYPOINT ["/app"]
