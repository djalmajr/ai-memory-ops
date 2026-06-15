# Keycloak realm bootstrap (clients + roles)

`ai-memory-svc` behind `mcp-auth` expects a Keycloak realm with three clients
and the `mcp:read` / `mcp:write` roles. This is a **one-time per realm** step —
the chart references the realm but does not create it. [`scripts/kc-bootstrap.sh`](scripts/kc-bootstrap.sh)
provisions it idempotently and generically (no instance-specific value is
hardcoded — everything is an env var).

## What it provisions

| Client | Type | Used by |
|---|---|---|
| `ai-memory-oauth2-proxy` | confidential, standard flow, PKCE S256 | `oauth2-proxy` interactive `/web` login |
| `ai-memory-mcp` | public, standard flow + DCR, PKCE S256 | MCP browser login (Claude Code / Codex / …) |
| `ai-memory-cli` | **public, device-flow only, no PKCE** | **CLI / headless lifecycle hooks** (`ai-memory auth login oidc-device`) |

Plus realm roles `mcp:read` / `mcp:write` and a group (`ai-memory`) that carries
them. `mcp-auth` rejects a token without the role (`403 missing_role`), so each
human is added to the group (or granted the roles directly).

> The `ai-memory-cli` client is the one **developers' hooks** need. The CLI
> device flow sends no PKCE, so it must be a **separate** client that does not
> enforce PKCE — reusing the PKCE-enforced `ai-memory-mcp` returns
> `400 Missing parameter: code_challenge_method`. A missing/disabled client is
> why `ai-memory auth login oidc-device --client-id ai-memory-cli` fails with
> `invalid_client` / `device flow is disabled`.

## When to run it

After `helm install ai-memory-svc` (the realm must already exist in your
Keycloak), and **before** onboarding developers. Re-running is safe (idempotent).

## How to run it

### Option A — by hand, inside the Keycloak pod (recommended; no secret leaves the cluster)

The admin credentials come from the pod's own `KEYCLOAK_ADMIN` /
`KEYCLOAK_ADMIN_PASSWORD` env, so you never copy them out:

```sh
POD=$(kubectl -n <keycloak-ns> get pods -l app=keycloak -o name | head -1)   # adjust selector
kubectl -n <keycloak-ns> exec -i "$POD" -- sh -c \
  'KC_REALM=<realm> PUBLIC_URL=https://<host>[/base-path] \
   KC_SERVER=http://localhost:8080/auth \
   KC_ADMIN_USER="$KEYCLOAK_ADMIN" KC_ADMIN_PASS="$KEYCLOAK_ADMIN_PASSWORD" sh -s' \
  < charts/ai-memory-svc/scripts/kc-bootstrap.sh
```

- `KC_REALM` — the realm in your `mcpAuth.oidc.issuer` (`…/realms/<realm>`).
- `PUBLIC_URL` — the public base URL **including any base path**, no trailing
  slash (e.g. `https://<host>` or `https://<host>/wiki`). Used for the
  oauth2-proxy `redirectUri`/`webOrigins` and the MCP `redirectUris`.
- `KC_SERVER` — drop `/auth` on Keycloak builds with no relative path (a
  `404 Not Found` at login means flip it).

Windows admins: `Get-Content charts/ai-memory-svc/scripts/kc-bootstrap.sh | kubectl -n <ns> exec -i $POD -- sh -c '…'`.

### Option B — optional Helm hook Job (declarative)

Set `keycloakBootstrap.enabled=true` (off by default) to run the same script as
a `post-install`/`post-upgrade` Helm hook Job. It needs a Keycloak admin secret
and reachability to the Keycloak service from the cluster — see the
`keycloakBootstrap` block in `values.yaml`.

## Developer onboarding (after bootstrap)

Each developer then runs the device login + hook install once — see the
`aim-init` skill's "Keycloak-gated instance" section (the agent drives it):
`ai-memory auth login oidc-device --issuer <issuer> --client-id ai-memory-cli`
then `ai-memory install-hooks --apply --agent <agent> --server-url <PUBLIC_URL>`
(no static token). The developer needs `mcp:read`/`mcp:write` (via the group).
