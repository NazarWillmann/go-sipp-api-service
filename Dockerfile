# syntax=docker/dockerfile:1.6

# -------------------------
# Build Go application
# -------------------------
FROM golang:1.24.1-bookworm AS builder

WORKDIR /src

COPY go.mod ./

COPY cmd ./cmd
COPY internal ./internal
COPY openapi.yaml ./openapi.yaml
COPY configs ./configs

RUN --mount=type=cache,target=/go/pkg/mod \
    go mod tidy

RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -o /out/sipp-emulator ./cmd/server

# Runtime image (contains SIPp + this service)

# NOTE: Debian bookworm repositories may not include 'sipp' package by default.
# Alpine has a maintained 'sipp' package, so runtime uses Alpine for reliability.
FROM alpine:3.20 AS runtime

RUN apk add --no-cache ca-certificates sipp

WORKDIR /app
COPY --from=builder /out/sipp-emulator /app/sipp-emulator
COPY configs /app/configs
COPY openapi.yaml /app/openapi.yaml

ENV API_BIND=0.0.0.0:8080
EXPOSE 8080/tcp

ENTRYPOINT ["/app/sipp-emulator"]
