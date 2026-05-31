# Runbook 02 — Secrets bootstrap

The chart **never templates real credentials** (`secrets.create=false` by default).
Provision the secrets out-of-band before installing.

## Main secret — `<release>-secrets`
Name pattern: `<release>-secrets` (the deployment mounts it via `envFrom`, `optional: true`).
For a release named `ai-memory-svc` in `knowledge-center`:

```bash
kubectl -n knowledge-center create secret generic ai-memory-svc-secrets \
  --from-literal=AI_MEMORY_AUTH_TOKEN="$(openssl rand -hex 24)" \
  --from-literal=LLM_API_KEY="<llm-key-or-omit>" \
  --from-literal=OPENAI_API_KEY="<embeddings-key-or-omit>"
```

| Key | Used by | Notes |
|-----|---------|-------|
| `AI_MEMORY_AUTH_TOKEN` | engine `require_bearer` + mcp-auth `UPSTREAM_AUTH_TOKEN` | Static 2nd-layer token. mcp-auth swaps a valid JWT for this before the engine. Empty = no static layer. |
| `LLM_API_KEY` | engine consolidation | Omit for zero-LLM. |
| `OPENAI_API_KEY` | engine embeddings | Omit for FTS5-only. |
| `GITLAB_API_TOKEN` | ETL clone/push | Only if ETL enabled. |
| `OPENCODE_API_KEY` | ETL opencode | Only if ETL enabled. |

## oauth2-proxy secrets (only if `oauth2Proxy.enabled=true`)
Two pre-existing secrets, names set in the Questions form:
- **config** (`oauth2-proxy-config`): keys `oauth2-proxy.yaml` (alpha-config) +
  `cookie-secret` (`openssl rand -hex 16`). The alpha-config MUST set, in the OIDC
  provider block: `code_challenge_method: S256` (Keycloak PKCE), `audienceClaims: [aud]`,
  `emailClaim/userIDClaim: email`.
- **ca** (`oauth2-proxy-ca`): key `ca.crt` — the internal CA to trust a self-signed Keycloak.

## git-mirror SSH key (only if `webhooks.gitMirror.enabled` with `auth: ssh`)
Secret `<release>-git-mirror-ssh` (or a custom name) with `id_ed25519` (private deploy
key) + `known_hosts` (remote fingerprint).

## Hygiene
- `.gitleaks.toml` + a pre-commit hook guard against committing secrets.
- `values-*.yaml` are gitignored by design — never commit deploy-specific values.
