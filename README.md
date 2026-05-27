# ai-memory-ops

Self-host [**ai-memory**](https://github.com/akitaonrails/ai-memory) on Kubernetes: a Helm
chart plus the supporting container images to run it behind OIDC auth, with an optional
git→wiki ETL and a custom web UI.

## What's in here

| Path | What it is |
|------|------------|
| `charts/ai-memory-svc/` | Helm chart: deploys ai-memory + mcp-auth sidecar + ETL CronJob + optional oauth2-proxy + Traefik ingress |
| `images/ai-memory/` | Builds the ai-memory engine (and bakes in an optional custom web-UI SPA via `--web-ui-dir`) |
| `images/mcp-auth/` | Tiny Go sidecar: validates Keycloak/OIDC JWTs at the edge (Traefik `forwardAuth`) and injects the upstream bearer token for `/mcp` and `/hook` |
| `images/etl/` | Git→wiki ETL: clones source repos and ingests them into ai-memory as wiki pages (run as a CronJob) |
| `images/mcp-write/` | Optional MCP write proxy (durable page writes) |
| `deploy/rbac-deployer.yaml` | Scoped `Role`/`RoleBinding` so a CI service account can `helm upgrade` the release without cluster-admin |
| `examples/` | Example MCP client config |

## Architecture

```
            ┌── /web  ──▶ oauth2-proxy ──┐
client ──▶  Traefik ingress              ├──▶ ai-memory (engine + SPA)
            └── /mcp  ──▶ mcp-auth ──────┘         ▲
                         (JWT validate,            │ git ETL (CronJob)
                          inject bearer)           │
                                          source repos ─┘
```

- **`/mcp`** is for machines: `mcp-auth` validates the OIDC JWT and swaps it for the static
  `AI_MEMORY_AUTH_TOKEN` the engine expects.
- **`/web`** is for browsers: `oauth2-proxy` handles the interactive OIDC login.
- **ETL** ingests one or more git repos into ai-memory wiki pages on a schedule.

## Quick start

```bash
# 1. Provide secrets out-of-band (the chart does NOT template real secrets)
kubectl create namespace ai-memory
kubectl -n ai-memory create secret generic ai-memory-svc-secrets \
  --from-literal=AI_MEMORY_AUTH_TOKEN="$(openssl rand -hex 24)" \
  --from-literal=LLM_API_KEY="<your-llm-key>" \
  --from-literal=OPENAI_API_KEY="<embeddings-key-or-dummy>"

# 2. Install (override values for your registry / hosts / OIDC)
helm upgrade --install ai-memory-svc charts/ai-memory-svc \
  --namespace ai-memory \
  --set aiMemory.image.repository=registry.example.com/my-org/ai-memory
```

See [`charts/ai-memory-svc/README.md`](charts/ai-memory-svc/README.md) and the inline comments in
`charts/ai-memory-svc/values.yaml` for the full configuration surface.

## Secrets & config

- The chart **never** templates real credentials. Set `secrets.create: false` (default) and
  create the secret out-of-band, as above.
- Environment-specific Helm overrides (`values-*.yaml`) are **gitignored** by design — keep your
  deploy-specific values out of the repo.
- A `.gitleaks.toml` config + a `pre-commit` hook keep secrets from being committed.

## Custom web UI

The `ai-memory` image can bundle a static SPA served at `/web`. A reference frontend (SolidJS)
built against the read-only `/api/v1` contract lives at
[**ai-memory-ui**](https://github.com/djalmajr/ai-memory-ui).

## License

The ai-memory engine is licensed upstream. This packaging is provided as-is; add a license file
to suit your use.
