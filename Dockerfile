# syntax=docker/dockerfile:1

ARG GO_VERSION=1.26.4

# Build on the host platform and cross-compile a static binary for the requested
# target platform. Copying module files first preserves the dependency layer
# when application source code changes.
FROM --platform=$BUILDPLATFORM golang:${GO_VERSION}-bookworm AS builder

WORKDIR /src
ENV GOTOOLCHAIN=local

COPY go.mod go.sum ./
RUN go mod download && go mod verify

COPY cmd/ ./cmd/
COPY internal/ ./internal/

ARG TARGETOS
ARG TARGETARCH
ARG TARGETVARIANT
ARG VERSION=development
ARG COMMIT=unknown

# Docker distinguishes 32-bit ARM variants in the target platform. Go uses
# GOARM instead, so map both v6 and v7 explicitly rather than accidentally
# publishing the same binary under two architecture descriptors.
RUN set -eu; \
    if [ "$TARGETARCH" = "arm" ]; then \
      case "$TARGETVARIANT" in \
        v6) GOARM=6 ;; \
        v7|"") GOARM=7 ;; \
        *) echo "unsupported ARM variant: $TARGETVARIANT" >&2; exit 1 ;; \
      esac; \
      export GOARM; \
    fi; \
    CGO_ENABLED=0 GOOS="$TARGETOS" GOARCH="$TARGETARCH" \
      go build -mod=readonly -trimpath \
      -ldflags="-s -w -X main.version=${VERSION} -X main.commit=${COMMIT}" \
      -o /out/mysql-mcp ./cmd/mysql-mcp

# The scratch runtime contains no package manager or shell. The CA bundle is
# retained so TLS connections to MySQL can validate public certificate chains.
FROM scratch AS runtime

ARG VERSION=development
ARG COMMIT=unknown
LABEL org.opencontainers.image.title="mysql-mcp" \
      org.opencontainers.image.description="Policy-controlled MCP access to MySQL 5.7 and 8.x" \
      org.opencontainers.image.source="https://github.com/MegaXChan/mysql-mcp" \
      org.opencontainers.image.version=$VERSION \
      org.opencontainers.image.revision=$COMMIT

COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=builder /out/mysql-mcp /usr/local/bin/mysql-mcp

# Docker creates the working directory even though the scratch base is empty.
# Operators mount the deployment configuration at config.yaml beneath it.
WORKDIR /etc/mysql-mcp

# 65532 is the conventional non-root/nobody UID used by minimal containers.
USER 65532:65532

EXPOSE 8080
STOPSIGNAL SIGTERM

ENTRYPOINT ["/usr/local/bin/mysql-mcp"]
CMD ["serve", "--config", "/etc/mysql-mcp/config.yaml"]
