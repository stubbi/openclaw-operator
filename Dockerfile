# Build stage
FROM golang:1.27.3-alpine AS builder

ARG TARGETOS
ARG TARGETARCH
ARG TARGETPLATFORM
# When set, skip compilation and use the pre-built binary instead (used by GoReleaser)
ARG PREBUILT_BINARY

WORKDIR /workspace

# Copy dependency files first for caching
COPY go.mod go.sum ./
RUN if [ -z "$PREBUILT_BINARY" ]; then go mod download; fi

# Copy everything else (source code, and GoReleaser platform dirs if present)
COPY . .

# Use pre-built binary if available, otherwise build from source.
# GoReleaser (dockers_v2) places binaries under $TARGETPLATFORM/ (e.g., linux/amd64/manager).
RUN if [ -n "$PREBUILT_BINARY" ] && [ -f "${TARGETPLATFORM}/${PREBUILT_BINARY}" ]; then \
      cp "${TARGETPLATFORM}/${PREBUILT_BINARY}" manager; \
    else \
      CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH:-amd64} go build -a -o manager cmd/main.go; \
    fi

# Runtime stage - use distroless for minimal attack surface
FROM gcr.io/distroless/static:nonroot

WORKDIR /

COPY --from=builder /workspace/manager .

USER 65532:65532

ENTRYPOINT ["/manager"]
