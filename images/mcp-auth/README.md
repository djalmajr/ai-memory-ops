# ai-memory-svc-mcp-auth

JWT validator sidecar for **Traefik forwardAuth** — protects the MCP server HTTP endpoint. Tokens are JWTs issued by a Keycloak/OIDC realm; on success the sidecar injects the upstream bearer token.

## Stack

| Component | Version | Why |
|---|---|---|
| Go | 1.23 | LTS, static build, no CGO |
| `golang-jwt/jwt/v5` | ^5.2 | JWT parse + validation (RS256) |
| `MicahParks/keyfunc/v3` | ^3.3 | JWKS fetcher with auto-refresh cache |
| Base runtime | `scratch` | ~5-7 MB image, no libc/sh |
| Binary user | UID 65532 (nonroot) | No privileges |

## Endpoints

| Path | Auth | Status | Notes |
|---|---|---|---|
| `GET /healthz` | none | 200 always | k8s liveness probe |
| `GET /readyz` | none | 200 if JWKS already loaded, 503 otherwise | k8s readiness probe |
| `GET /verify` | bearer JWT | 200/401/403 | Traefik forwardAuth endpoint |

### `/verify` — flow

Traefik calls with headers `X-Forwarded-Method`, `X-Forwarded-Uri`, `X-Forwarded-Host`, `X-Forwarded-For`, and the original `Authorization`.

1. Path in allowlist (probes `/healthz`, `/readyz`)? → 200 without checking the token.
2. `Authorization: Bearer <jwt>` header missing? → **401**.
3. `HOOK_AUTH_TOKEN` set + route is `/hook` or `/handoff` + bearer matches it
   (constant-time)? → **200** with `X-Memory-Actor-User`/`X-Auth-Username` from
   `HOOK_AUTH_USERNAME` and the upstream bearer injected. Anywhere else the same
   token falls through to JWT validation below (and fails).
4. JWT parse fails / invalid sig / expired / issuer ≠ `OIDC_ISSUER`? → **401**.
5. `OIDC_AUDIENCE` configured and `aud` claim does not match? → **401**.
6. Route requires `mcp:write` but claims do not have it? → **403**.
7. OK → useful response headers (`X-Auth-Email`, `X-Auth-Username`,
   `X-Auth-Sub`, `X-Memory-Actor-User`, `X-Memory-Actor-Sub`,
   `X-Memory-Actor-Client`, `X-Memory-Actor-Agent`) → **200**.

The Keycloak/OIDC `sid` claim is not propagated as
`X-Memory-Actor-Session-Id`. ai-memory reserves that header for the real
lifecycle-hook session id from a coding-agent run; a provider login session is a
different concept.

### Route → role mapping

```text
GET  /wiki/mcp/...                → mcp:read
POST /wiki/mcp/...                → mcp:read   (JSON-RPC tools/list, tools/call read-only)
POST /wiki/mcp/write/...          → mcp:write   (#5 — future)
POST /wiki/mcp/admin/...          → mcp:write
DELETE|PUT|PATCH /wiki/mcp/...    → mcp:write
```

Conservative: everything is `mcp:read` until proven otherwise; explicit write routes are opt-in.

## Env vars

| Variable | Required | Default | Description |
|---|---|---|---|
| `OIDC_ISSUER` | **yes** | — | `https://keycloak.example.com/realms/ai-memory-svc` (lab) |
| `OIDC_AUDIENCE` | no | `""` (not checked) | If set, requires the `aud` claim in the JWT |
| `HOOK_AUTH_TOKEN` | no | `""` (off) | Static bearer accepted ONLY on `/hook` and `/handoff` (agent lifecycle hooks are headless — no interactive OAuth). Constant-time compare |
| `HOOK_AUTH_USERNAME` | no | `""` | Username propagated as `X-Auth-Username` / `X-Memory-Actor-User` when the hook token matches |
| `PORT` | no | `8081` | validator HTTP port |
| `LOG_LEVEL` | no | `info` | `debug` / `info` / `warn` / `error` |
| `JWKS_REFRESH_SECONDS` | no | `300` | JWKS cache TTL |

## Build local

```bash
cd images/mcp-auth
go build -o /tmp/mcp-auth .
OIDC_ISSUER=https://keycloak.example.com/realms/ai-memory-svc /tmp/mcp-auth
# em outro terminal:
curl -sS -o /dev/null -w "%{http_code}\n" http://localhost:8081/healthz   # 200
curl -sS -o /dev/null -w "%{http_code}\n" http://localhost:8081/verify    # 401 (sem Authorization)
```

## Build container (multi-arch)

```bash
docker buildx build \
  --platform linux/arm64,linux/amd64 \
  -t registry.example.com/my-org/ai-memory-svc-mcp-auth:0.1.0 \
  --push \
  images/mcp-auth/
```

For lab arm64 only:

```bash
docker buildx build --platform linux/arm64 \
  -t ai-memory-svc-mcp-auth:0.1.0 \
  --load images/mcp-auth/
```

Local smoke test with a real Keycloak:

```bash
docker run --rm -d --name mcp-auth -p 8081:8081 \
  -e OIDC_ISSUER=https://keycloak.example.com/realms/ai-memory-svc \
  -e LOG_LEVEL=debug \
  --add-host keycloak.example.com:<keycloak-ip> \
  ai-memory-svc-mcp-auth:0.1.0

# Get a real JWT (Direct Access Grants from the mcp-cli client)
TOKEN=$(curl -sS -X POST https://keycloak.example.com/realms/ai-memory-svc/protocol/openid-connect/token \
  -H "Content-Type: application/x-www-form-urlencoded" \
  -d "grant_type=password&client_id=mcp-cli&username=<user>&password=<pass>" \
  | jq -r '.access_token')

# Simulate Traefik forwardAuth
curl -sS -o /dev/null -w "%{http_code}\n" http://localhost:8081/verify \
  -H "Authorization: Bearer $TOKEN" \
  -H "X-Forwarded-Method: POST" \
  -H "X-Forwarded-Uri: /wiki/mcp/tools/call"
# 200 (role mcp:read present) or 403 (role missing)
```

## How it works in the chart (sidecar)

The `ai-memory-svc` helm chart adds this container as a **sidecar** of the MCP pod. A Traefik middleware points to `mcp-auth:8081/verify`. See `charts/ai-memory-svc/templates/deployment.yaml` + `templates/middleware-forwardauth.yaml`.

## Size

A local arm64 build produces a ~9.7 MB binary; with `-ldflags="-s -w"` in the Dockerfile it drops to ~6-7 MB. The final scratch image is ~5-7 MB.

## Observability

JSON-line logs via `log/slog`. Each `/verify` call produces 1 line with:

- `verify_ok` (200) / `verify_unauthorized` (401) / `verify_forbidden` (403)
- `sub`, `uri`, `method`, `ip`, `elapsed_ms`
- **Never the token**

Logs go to stdout — k8s collects them automatically.

## Remaining risks

- **`OIDC_ISSUER` over HTTPS** — public OIDC servers are covered by the Mozilla CA bundle from the builder stage. For a self-signed OIDC CA, append your `ca.crt` to the bundle in the builder.
- **Network policy** — none by default; consider restricting `mcp-auth` to Traefik-only traffic.
- **Replay** — Keycloak JWTs expire in 15min (configurable in the realm).
