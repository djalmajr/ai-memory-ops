# Runbook 03 — Ingress auth: Traefik vs nginx (transparent)

The chart works on both a Traefik cluster and an nginx-only cluster **without a
value change**. This explains how, and how edge auth is wired on each.

## Backend resolution
`ingress.className` accepts `auto | traefik | nginx` (default `auto`). The helper
`ai-memory-svc.ingressBackend` resolves it:

```
auto  → traefik  if the cluster has the CRD traefik.io/v1alpha1
      → nginx    otherwise
traefik / nginx  → used verbatim (force an override)
```

Every template branches on this resolved value, never on the raw `className`. So the
same chart artifact is portable: a Traefik lab resolves to Traefik; the Serpro
ingress-nginx cluster resolves to nginx.

## Auth model per backend
ai-memory serves at the root: `/mcp` + `/hook` (machines), `/web` + `/api/v1`
(browsers), `/` (SPA) and `/.well-known/...` (public).

| Concern | Traefik | nginx |
|---------|---------|-------|
| `/mcp`, `/hook` JWT validation | `Middleware` forwardAuth → mcp-auth `/verify` | per-route `Ingress` with `auth-url` → mcp-auth `/verify` + `auth-response-headers` |
| `/web`, `/api/v1` browser login | `IngressRoute` (priority) → oauth2-proxy | per-route `Ingress` with `auth-url`/`auth-signin` → oauth2-proxy |
| `/oauth2/*` (callback) | covered by the IngressRoute | dedicated `Ingress`, no auth |
| prefix strip (path-based) | `StripPrefix` Middleware | `rewrite-target: <route>/$2` per Ingress |

On nginx, the more-specific per-route Ingresses win over the base `/` Ingress (which
stays unauthenticated for the SPA + public metadata).

## Why mcp-auth is ingress-agnostic
Traefik forwardAuth sends `X-Forwarded-Method` / `X-Forwarded-Uri`; nginx
`auth_request` sends `X-Original-Method` / absolute `X-Original-URL`. The Serpro
ingress-nginx ConfigMap has `allow-snippet-annotations: false`, so we **cannot** use
an `auth-snippet` to synthesize the Traefik headers. Instead the sidecar
(`images/mcp-auth/main.go` → `forwardedRoute`) reads **both** flavors, normalizing the
nginx absolute URL down to path+query. No per-cluster config needed.

## Operator notes
- Prefer **`Base Path = /`** (host-dedicated). Path-based (`/wiki`) works via
  rewrite, but oauth2-proxy redirect/cookie paths are simpler at root.
- `AI_MEMORY_ALLOWED_HOSTS` auto-appends `ingress.host`, so no manual allow-list edit.
- ingress-nginx auth-response-headers carry the swapped `Authorization` (static
  `AI_MEMORY_AUTH_TOKEN`) and the `X-Memory-Actor-*` headers the contributors webhook uses.
