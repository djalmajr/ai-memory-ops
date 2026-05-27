# ai-memory-svc Helm chart

Helm chart for **ai-memory-svc** — a single pod with the MCP server and an ETL sidecar with an internal cron. Together they form a Living Knowledge Base (Wiki + ADRs + Rules + History) versioned in Git and queryable via MCP.

## Architecture

| Resource | Kind | Purpose |
|---|---|---|
| `ai-memory-svc` | Deployment (replicas: 1, strategy: Recreate) | Pod with 2 containers — `mcp` (long-running, serves HTTP/SSE queries) + `etl-cron` (supercronic + ETL script) |
| `ai-memory-svc` | Service (ClusterIP) | Front for the `mcp` container |
| `ai-memory-svc-data` | PVC (`ReadWriteOnce`) | SQLite + wiki clone + models cache, mounted at `/data` |
| `ai-memory-svc-config` | ConfigMap | Runtime config (paths, repo URL, model, ETL schedule) |
| `ai-memory-svc-secrets` | Secret (referenced externally) | `GITLAB_API_TOKEN`, `OPENCODE_API_KEY`, `MCP_AUTH_TOKEN` |
| `ai-memory-svc` | Ingress (optional) | Public endpoint with auth for MCP clients |

**Topology (RWO single-pod with sidecar):** SQLite WAL on the same kernel supports 1 writer (ETL) + N readers (MCP) without distributed locking. RWO storage works on any provider (local-path block storage, EBS, OCI block, etc.) — it does not require RWX/NFS.

## Prerequisites

- Kubernetes 1.27+
- Helm 3.13+
- Ingress controller (e.g. ingress-nginx, Traefik)
- (Optional) cert-manager + ClusterIssuer for automatic TLS on the Ingress
- StorageClass with `ReadWriteOnce` (any provider; SSD recommended)
- 1 external Secret in the target namespace with the 3 keys:
  - `GITLAB_API_TOKEN` — `read_repository` + `write_repository` scope on the target repo
  - `OPENCODE_API_KEY` — OpenCode plan key
  - `MCP_AUTH_TOKEN` — random bearer token (validated by the MCP)

## Installation (CLI)

```bash
# 1. Create namespace and Secret externally (not managed by the chart by design)
kubectl create namespace ai-memory
kubectl -n ai-memory create secret generic ai-memory-svc-secrets \
  --from-literal=GITLAB_API_TOKEN="$GITLAB_API_TOKEN" \
  --from-literal=OPENCODE_API_KEY="$OPENCODE_API_KEY" \
  --from-literal=MCP_AUTH_TOKEN="$(openssl rand -hex 32)"

# 2. Validate and install
helm lint .
helm upgrade --install ai-memory-svc . \
  --namespace ai-memory \
  --wait --timeout 5m
```

## Installation (Rancher UI)

1. Under the cluster's **Apps → Repositories**, add the Git repo containing this chart.
2. Under **Apps → Charts**, filter by the added repo and open `ai-memory-svc`.
3. Click **Install** — Rancher's interactive form renders the 6 groups from `questions.yaml` (Wiki Configuration, MCP Server, ETL Pipeline, Persistence, Networking, Secrets).
4. Before clicking the final **Install**: create the `Secret ai-memory-svc-secrets` in the target namespace (Rancher does not create `Opaque` Secrets via the UI by design).

## Main values

| Path | Default | Description |
|---|---|---|
| `config.wiki.repoUrl` | `https://gitlab.example.com/<group>/wiki-content.git` | Git URL of the ETL target repo (replace with your own) |
| `config.wiki.branch` | `main` | Target branch |
| `config.wiki.sources[]` | `[]` | List of source repos `{name, repoUrl, branch}` that the ETL ingests |
| `config.language` | `pt-BR` | Content language (read by the `wiki-ingest`/`wiki-lint` skills) |
| `config.openCode.model` | `opencode/kimi-k2.6` | OpenCode model in the `<provider>/<model>` format |
| `mcp.image.repository` / `tag` | placeholder (`nginx:1.27-alpine` in the pilot) | MCP server image |
| `etl.enabled` | `true` | Enables the ETL sidecar |
| `etl.schedule` | `0 0 * * *` | 5-field supercronic cron |
| `etl.image.repository` / `tag` | configure with your registry | ETL image |
| `etl.runIndexer` | `true` | The ETL also runs the indexer (SQLite rebuild) at the end |
| `etl.git.userName` / `userEmail` | `ai-memory-bot` | Identity of the bot's commits |
| `persistence.accessMode` | `ReadWriteOnce` | The topology requires RWO; do **not** change to RWX |
| `persistence.storageClass` | `""` (cluster default) | Specify your provider's StorageClass |
| `persistence.size` | `20Gi` | SQLite + wiki clone + models cache |
| `service.type` | `ClusterIP` | Service type |
| `ingress.enabled` | `false` | Enables Ingress (requires `host` set) |
| `ingress.className` | `nginx` | `nginx`, `traefik`, or another available IngressClass |
| `ingress.host` | `""` | Hostname (e.g. `wiki.example.com`) |
| `ingress.path` | `/` | `/` for hostname-based; `/wiki` for path-based |
| `ingress.tls.enabled` | `false` | TLS via cert-manager (annotation `cert-manager.io/cluster-issuer`) |
| `imagePullSecrets[]` | `[]` | For private registries — reference already-created `docker-registry` Secrets |
| `secrets.create` | `false` | Keep `false` in prod; provision the Secret externally |

See [`values.yaml`](values.yaml) for details and the full structure.

## Operation

### Trigger ETL on-demand

```bash
kubectl -n ai-memory exec deploy/ai-memory-svc -c etl-cron -- \
  /usr/local/bin/entrypoint.sh run
```

### Follow logs

```bash
# MCP
kubectl -n ai-memory logs -l app.kubernetes.io/name=ai-memory-svc -c mcp -f --tail=100

# ETL (supercronic + run-etl)
kubectl -n ai-memory logs -l app.kubernetes.io/name=ai-memory-svc -c etl-cron -f --tail=100
```

### Release status

```bash
helm -n ai-memory status ai-memory-svc
helm -n ai-memory history ai-memory-svc
```

### Change the schedule without losing overrides

```bash
helm -n ai-memory upgrade ai-memory-svc . \
  --reuse-values \
  --set etl.schedule='*/15 * * * *'
```

## Integration with MCP clients

Once the Ingress is reachable with valid TLS, configure clients (Claude Desktop, Claude Code, etc.) with:

- **URL:** `https://<your-host>/<your-path>`
- **Auth header:** `Authorization: Bearer <MCP_AUTH_TOKEN>`

The `MCP_AUTH_TOKEN` is the value stored in the `ai-memory-svc-secrets` Secret (key `MCP_AUTH_TOKEN`).

## Compatibility

Works on any Kubernetes distribution (k3s, RKE2, EKS, GKE, AKS, Rancher-managed, etc.) with:

- A compatible Ingress controller (Traefik, ingress-nginx, etc.)
- A StorageClass with `ReadWriteOnce`
- ETL (and MCP, when real) images pullable by the cluster — use `imagePullSecrets` if the registry requires auth

No dependencies on a specific cloud provider or proprietary resource.
