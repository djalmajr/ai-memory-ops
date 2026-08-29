# ai-memory-svc-mcp-auth

JWT validator sidecar for **Traefik forwardAuth** — protects the MCP server HTTP endpoint. Tokens are JWTs issued by a Keycloak/OIDC realm; on success the sidecar injects the upstream bearer token.

## Stack

| Component | Version | Why |
|---|---|---|
| Go | 1.23 | LTS, static build, no CGO |
| `golang-jwt/jwt/v5` | ^5.2 | JWT parse + validation (RS256) |
| `MicahParks/keyfunc/v3` | ^3.3 | JWKS fetcher with auto-refresh cache |
| `modernc.org/sqlite` | v1.38.2 | Pure-Go sqlite (`CGO_ENABLED=0`, scratch). Pinned below Go 1.24+. |
| Base runtime | `scratch` | ~5-7 MB image, no libc/sh |
| Binary user | UID 65532 (nonroot) | No privileges |

## Endpoints

| Path | Auth | Status | Notes |
|---|---|---|---|
| `GET /healthz` | none | 200 always | k8s liveness probe |
| `GET /readyz` | none | 200 if JWKS already loaded, 503 otherwise | k8s readiness probe |
| `GET /verify` | bearer JWT / `amk_` / hook | 200/401/403 | Traefik forwardAuth endpoint |
| `GET /keys` | identified + `mcp:admin` | `{ "keys": ConsumerKey[] }` | 404 when `KEYS_DB` unset |
| `POST /keys` | identified + `mcp:admin` | 201 `ConsumerKey` + plaintext `key` once | owner derived from caller, never the body |
| `GET /keys/whoami` | any | `{ "identity": KeyOwner\|null, "can_issue": bool }` | fail-closed UI field; 404 when unset |
| `DELETE /keys/{id}` | identified + `mcp:admin` | 204 | idempotent soft revoke |

**Routing:** this sidecar now serves `/keys*`, but the edge does **not** route that prefix today (helm does not map it; production compose is server-only). The SPA same-origin path `${basePath}/keys` only works after the edge forwards `/keys*` here. No CORS layer — the UI is same-origin behind that edge.

**Privilege to issue/manage:** a JWT with realm role `mcp:admin` (create it in Keycloak), or an `amk_` key with scope `admin`. If the realm cannot gain a new role, set `KEYS_ADMIN_SUBJECTS` to a comma-separated list of `issuer|subject` pairs. Hook token and anonymous callers get `403 {"error":"issuance requires an identified operator"}`.

### `/verify` — flow

Traefik calls with headers `X-Forwarded-Method`, `X-Forwarded-Uri`, `X-Forwarded-Host`, `X-Forwarded-For`, and the original `Authorization`.

1. Path in allowlist (probes `/healthz`, `/readyz`)? → 200 without checking the token.
2. `Authorization: Bearer <jwt>` header missing? → **401**.
3. `HOOK_AUTH_TOKEN` set + route is `/hook` or `/handoff` + bearer matches it
   (constant-time)? → **200** with `X-Memory-Actor-User`/`X-Auth-Username` from
   `HOOK_AUTH_USERNAME` and the upstream bearer injected. Anywhere else the same
   token falls through to JWT validation below (and fails).
4. Bearer starts with `amk_` and `KEYS_DB` is set? Look up `sha256(secret)`.
   Unknown / revoked / expired → **401**. Scope too low for the route → **403**.
   OK → **200** with `Authorization: Bearer ${ACTOR_PROXY_BEARER_TOKEN}` and
   `X-Memory-Actor-User: <actor_user>`. `X-Memory-Actor-Issuer` + `X-Memory-Actor-Sub`
   are sent **only** when the key has scope `admin` and its owner `kind=subject`
   (both values, never a partial pair — the engine 400s otherwise). Headers **replace**
   client-supplied actor values (`Set`, never `Add`).
5. JWT parse fails / invalid sig / expired / issuer ≠ `OIDC_ISSUER`?
   If `PASSTHROUGH_UNKNOWN_BEARER=1` and the bearer was not an `amk_` key → **200**
   **echoing** the caller's own `Authorization` — never the upstream token, which
   would upgrade an unknown bearer to a valid one (engine rungs still apply; this
   is how current
   CLI tokens keep working during migration). Otherwise → **401**.
6. `OIDC_AUDIENCE` configured and `aud` claim does not match? → **401**.
7. Route requires `mcp:write` but claims do not have it? → **403**.
8. OK → useful response headers (`X-Auth-Email`, `X-Auth-Username`,
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

### Consumer keys (`amk_`)

Opt-in via `KEYS_DB`. Secret format: `amk_` + 40 hex chars from `crypto/rand` (160 bits). Only `sha256(secret)` hex and the last 4 chars are stored.

SHA-256 rather than argon2/bcrypt (the original issue text): the secret is full-entropy CSPRNG, not a user-chosen password; `/verify` needs an indexable O(1) lookup on every request; the engine already hashes `users.token_hash` with SHA-256.

Scope × route:

- `read` — real HTTP GET/HEAD (`/api/v1/*`, `/web/*`, …). Scope `read` covers HTTP reads; MCP access requires `write` because the JSON-RPC endpoint multiplexes read and write and forwardAuth does not see the tool name.
- `write` — everything in `read`, plus any MCP call (`POST /mcp`), `/hook`, `/handoff`, and other mutating methods (`PUT`/`DELETE`/`PATCH`)
- `admin` — everything in `write`, plus engine `/admin/` paths (not `/mcp/admin/`)

Follow-up (not in this sidecar): propagate the consumer-key scope into the engine and enforce per tool/capability there. Parsing JSON-RPC in forwardAuth is not an option — the hop does not get a reliable body.

`POST /keys` body: `{id, actor_user, scopes[], expires_at?}`. `id` matches `^[a-z0-9][a-z0-9-]{1,63}$`. A body that carries any `owner*` field is **400** — owner is always `callerIdentity` (JWT subject, or the issuing `amk_` key's stored owner).

## Env vars

| Variable | Required | Default | Description |
|---|---|---|---|
| `OIDC_ISSUER` | unless `KEYS_DB` is set | — | `https://keycloak.example.com/realms/ai-memory-svc` (lab). Empty + `KEYS_DB` set → **keys-only mode** (see below). Empty with no `KEYS_DB` is boot-fatal: nothing to validate |
| `OIDC_AUDIENCE` | no | `""` (not checked) | If set, requires the `aud` claim in the JWT |
| `HOOK_AUTH_TOKEN` | no | `""` (off) | Static bearer accepted ONLY on `/hook` and `/handoff` (agent lifecycle hooks are headless — no interactive OAuth). Constant-time compare |
| `HOOK_AUTH_USERNAME` | no | `""` | Username propagated as `X-Auth-Username` / `X-Memory-Actor-User` when the hook token matches |
| `KEYS_DB` | no | `""` (off) | Path to the consumer-keys sqlite file (default in the plan: `/data/keys.db`). Empty → `/keys*` is 404 and `amk_` is not recognised |
| `ACTOR_PROXY_BEARER_TOKEN` | when `KEYS_DB` is set | — | Injected as `Authorization: Bearer …` after a valid `amk_` key. Boot-fatal if missing while `KEYS_DB` is set. Must equal the engine's `actor_proxy_bearer_token` and be **DISTINCT** from `AI_MEMORY_AUTH_TOKEN` — see below |
| `PASSTHROUGH_UNKNOWN_BEARER` | no | `0` | `1` → bearer that is neither `amk_` nor a valid JWT gets 200 with the caller's own `Authorization` **echoed back** (CLI tokens during migration) |
| `UPSTREAM_AUTH_TOKEN` | no | `""` | Static bearer injected on the hook and OIDC branches, replacing the caller's. Unset → those branches echo the caller's own token, which does **not** enter the engine's trusted-proxy rung. Set it to the same value as `ACTOR_PROXY_BEARER_TOKEN` |
| `KEYS_ADMIN_SUBJECTS` | no | `""` | Comma-separated `issuer|subject` pairs allowed to manage keys when the realm cannot add `mcp:admin` |
| `PORT` | no | `8081` | validator HTTP port |
| `LOG_LEVEL` | no | `info` | `debug` / `info` / `warn` / `error` |
| `JWKS_REFRESH_SECONDS` | no | `300` | JWKS cache TTL |

### Keys-only mode (no identity provider)

A deployment whose job is per-consumer `amk_` keys needs no Keycloak. An `amk_`
key is resolved **locally** against `KEYS_DB` (`keys.go:607-616`) and a key with
scope `admin` can issue and revoke others, so an issuer would only gate an
install on a provider the sidecar never calls.

Leave `OIDC_ISSUER` empty with `KEYS_DB` set: JWKS init is skipped, the JWT
branch fails closed on every request (`parseJWT` reports `jwks_unavailable`
while `jwtKeyfunc` is nil), and `OAUTH_ENABLED` is forced off — the RFC 9728
metadata has no issuer to advertise. Boot logs `mode: keys-only`.

In this mode `KEYS_ADMIN_SUBJECTS` and the `mcp:admin` realm role are
inapplicable (both are issuer/subject based). Bootstrap the first admin key with
SQL, from **inside** the container or a container sharing the volume — never
with a host `sqlite3` against a bind mount while the sidecar holds the file
(WAL across a macOS bind mount gives `disk I/O error (1034)`).

Exercised with no provider running at all:

| Request | Result |
|---|---|
| `POST /keys` with no credential | 403, `can_issue:false` |
| `GET /keys/whoami` with the admin key | `can_issue:true`, owner `djalmajr` (kind `user`) |
| `POST /keys` with the admin key | key issued, owner **derived** from the caller |
| `/verify` read-only key → `GET /api/v1/workspaces` | 200 |
| `/verify` read-only key → `POST /mcp` | 403 |
| `/verify` read-only key → `GET /admin/status` | 403 |
| `/verify` admin key → `GET /admin/status` | 200 |
| `/verify` unknown bearer, passthrough on | 200 |
| `/healthz` / `/readyz` | 200 / `{"status":"ready"}` |

`/readyz` means "what this instance validates is loaded": the key store in
keys-only mode, the JWKS in OIDC mode. It is not relaxed — a keys-only store
never satisfies readiness for an OIDC instance whose JWKS failed.

### Why every 200 names `Authorization`

A `copy_headers`-style integration treats the listed headers as
**authoritative**: a header the auth response omits is **removed** from the
request. Verified with Caddy 2 `forward_auth` — a 200 that carried no
`Authorization` made the upstream see none at all, so the caller's bearer was
destroyed and every request 401'd.

"Leave it untouched" only holds for nginx `auth_request`, where the operator
copies headers by hand. So each 200 states the header explicitly:

| Branch | `Authorization` sent upstream |
|---|---|
| public path (`/healthz`, RFC 9728 metadata) | caller's own, echoed — injecting here would credential an unauthenticated caller |
| static hook token on `/hook`/`/handoff` | `UPSTREAM_AUTH_TOKEN` when set, else the caller's own |
| valid `amk_` consumer key | `ACTOR_PROXY_BEARER_TOKEN` |
| unknown bearer with passthrough on | caller's own, echoed — **never** the upstream token |
| valid OIDC JWT | `UPSTREAM_AUTH_TOKEN` when set, else the caller's own |

### The proxy token must be DISTINCT from the engine's root token

The engine honours `X-Memory-Actor-*` **only** on its trusted-proxy rung, and it
tests the root credential **first**: *"Rung 1: root credential. Actor assertion
headers are intentionally ignored here; only the distinct proxy credential may
assert them."* (`auth.rs:329-331`). So if the proxy token equals
`AI_MEMORY_AUTH_TOKEN`, root matches first and every translated identity lands as
**Root with no attribution** — silently.

Measured through the full chain (Caddy + this sidecar + a real engine), firing
`POST /hook` with the static hook token:

| sidecar `UPSTREAM_AUTH_TOKEN` | hook | `actor_user` recorded |
|---|---|---|
| unset | **401** | no session created |
| `proxytoken` (engine's `actor_proxy_bearer_token`) | 202 | `user:djalmajr` |
| `devtoken` (engine's `bearer_token`) | 202 | **empty — attribution lost** |

Rule: `ACTOR_PROXY_BEARER_TOKEN` and `UPSTREAM_AUTH_TOKEN` carry the **same
proxy token**, and that token is **never** the engine's `bearer_token`. The
engine's own config template says the same thing beside
`actor_proxy_bearer_token`.

Passthrough is unaffected — it never injects, by design.

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
