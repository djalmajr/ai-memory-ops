# ai-memory-svc-mcp-auth — JWT validator sidecar for Traefik forwardAuth.
#
# Lightweight container (~5 MB) that validates Keycloak JWTs via public JWKS + audience +
# realm-level role. No local state. Receives forwardAuth from Traefik and responds
# 200/401/403 based on the token's roles.
#
# Multi-arch build:
#   docker buildx build --platform linux/arm64,linux/amd64 \
#     -t registry.example.com/my-org/ai-memory-svc-mcp-auth:0.1.0 \
#     --push images/mcp-auth/
#
# Env vars (consumed by main.go):
#   OIDC_ISSUER         (required) — https://keycloak.example.com/realms/ai-memory-svc
#   OIDC_AUDIENCE       (optional) — expects this aud in the JWT
#   PORT                (default 8081)
#   LOG_LEVEL           (default info)
#   JWKS_REFRESH_SECONDS (default 300)

# ============================================================================
# Stage 1 — Builder
# ============================================================================
FROM golang:1.23-alpine AS builder

WORKDIR /src

# Copy go.mod + go.sum first for deps caching
COPY go.mod go.sum* ./
RUN go mod download

COPY main.go ./

# Static build — no CGO, no libc deps. -ldflags -s -w strips debug symbols.
ARG TARGETARCH
ARG TARGETOS=linux
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags="-s -w" -o /out/mcp-auth .

# ============================================================================
# Stage 2 — Runtime (scratch + CA bundle for HTTPS)
# ============================================================================
FROM scratch

# Version label
LABEL org.opencontainers.image.version=0.1.0 \
      org.opencontainers.image.title="ai-memory-svc-mcp-auth" \
      org.opencontainers.image.description="JWT validator sidecar (Keycloak OIDC) for the MCP server"

# CA bundle (Mozilla) for HTTPS to public OIDC servers.
# On scratch, /etc/ssl/certs must exist; we copy it from the builder.
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
# Optional: for self-signed OIDC, append your ca.crt to the bundle in the builder stage.

# Run as non-root (UID 65532 = "nonroot" comum em distroless)
USER 65532:65532

# Binary
COPY --from=builder /out/mcp-auth /usr/local/bin/mcp-auth

EXPOSE 8081

ENTRYPOINT ["/usr/local/bin/mcp-auth"]
