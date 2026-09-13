# syntax=docker/dockerfile:1

# ---------------------------------------------------------------------------
# Stage 1 -- build
#
# Dependencies are downloaded in their own layer so that editing source does not
# re-download the module cache. The build cache mounts keep repeat builds fast
# without baking anything into the final image.
# ---------------------------------------------------------------------------
FROM golang:1.23-alpine AS build

WORKDIR /src

COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

COPY . .

ARG VERSION=dev
# CGO_ENABLED=0 gives a fully static binary, which is what lets the runtime stage
# be distroless/static: no libc, no shell, nothing to exploit or to patch.
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=linux go build \
      -trimpath \
      -ldflags="-s -w -X main.version=${VERSION}" \
      -o /out/wallet ./cmd/wallet

# Prove the artifact works before it ships: a binary that cannot even print its
# own flags is not going to be discovered at 3am in production.
RUN /out/wallet --help 2>&1 | grep -q healthcheck

# ---------------------------------------------------------------------------
# Stage 2 -- runtime
#
# distroless/static:nonroot contains only CA certificates, /etc/passwd and tzdata.
# It runs as uid 65532 (nonroot) by default -- the container cannot become root
# because there is no setuid binary and no shell to run one.
# ---------------------------------------------------------------------------
FROM gcr.io/distroless/static-debian12:nonroot AS runtime

WORKDIR /app
COPY --from=build /out/wallet /app/wallet

# Explicit even though the base image already defaults to it: the next person to
# edit this file should have to delete a line to break it, not forget to add one.
USER nonroot:nonroot

ENV PORT=8080
EXPOSE 8080

# The binary probes itself -- see cmd/wallet/main.go. Exec form, so no shell is
# needed. start-period covers the database cold start on a free-tier host.
HEALTHCHECK --interval=30s --timeout=5s --start-period=40s --retries=3 \
  CMD ["/app/wallet", "-healthcheck"]

ENTRYPOINT ["/app/wallet"]
