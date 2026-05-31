# Runbook 01 — Install on a client Rancher via the Helm Charts UI

Audience: operator installing `ai-memory-svc` **manually** through the Rancher
**Apps / Helm Charts** UI, filling values via the **Questions** form (not a
`values-*.yaml`). Reference target: the Serpro Rancher cluster.

## 0. Preconditions
- Cluster reachable in Rancher; you have a project/namespace with deploy rights.
- Ingress controller present. The chart auto-detects: with no Traefik CRD it uses
  **ingress-nginx** (the Serpro case). See [runbook 03](03-ingress-auth-traefik-vs-nginx.md).
- Nodes are **linux/amd64** (the CI publishes single-arch amd64 images).
- A default StorageClass exists (Serpro: `oci-bv`, RWO).
- Images published to `ghcr.io/djalmajr/ai-memory-ops/*`. If the cluster has no
  egress to ghcr.io, mirror them into the client registry and adjust the image
  repositories in the Questions form (+ an imagePullSecret if private).

## 1. Register the Helm repository in Rancher
Rancher → **Apps → Repositories → Create**:
- Name: `ai-memory-ops`
- Target: the source that serves this chart (one of):
  - **Git repository** containing Helm chart(s): URL `https://github.com/djalmajr/ai-memory-ops`,
    branch `main`, and set the path to `charts/` if prompted; **or**
  - an **HTTP(S) Helm repo** (`index.yaml`) if you publish a packaged chart.

Wait for the repo to show `Active`. The chart **ai-memory-svc** (v0.2.0+) then
appears under **Apps → Charts**.

## 2. Create the namespace + secrets (out-of-band)
The chart does NOT template real credentials. Do [runbook 02](02-secrets-bootstrap.md)
first. Target namespace for Serpro: **`knowledge-center`**.

```bash
kubectl create namespace knowledge-center   # if it doesn't exist
# then the secret per runbook 02
```

## 3. Install via the Questions form
**Apps → Charts → ai-memory-svc → Install**. Pick namespace `knowledge-center`.
Fill the tabs:

- **Images & Registry** — keep the `ghcr.io/djalmajr/ai-memory-ops/*` defaults, or
  point to the mirrored registry. Set `imagePullSecrets` only if private.
- **ai-memory Engine** — `workspace`, `project` (set a stable seed name, not empty),
  resources.
- **LLM & Embeddings** — leave empty for zero-LLM + FTS5-only, or fill the provider
  endpoints.
- **Networking & Ingress** — `Enable Ingress = true`; `Ingress Class = auto`
  (resolves to nginx on Serpro); `Hostname` = the public DNS; `Base Path = /`
  (recommended) ; `Enable TLS` with cert-manager.
- **Authentication** — enable `mcp-auth` and set the **OIDC Issuer** (Keycloak realm)
  to protect `/mcp`. Enable `oauth2-proxy` only after its secrets exist (runbook 02).
- **Persistence** — `Storage Class = oci-bv` (or leave empty for the default), size.
- **ETL / Webhooks / Monitoring** — enable as needed.
- **Secrets (dev only)** — keep `Create Secret in chart = false` (you created it in step 2).

Click **Install** and watch the workload come up in `knowledge-center`.

## 4. Verify
```bash
kubectl -n knowledge-center get pods,ingress,pvc
kubectl -n knowledge-center logs deploy/<release>-ai-memory -c ai-memory
# mcp-auth sidecar (if enabled) — JWKS fetched:
kubectl -n knowledge-center logs deploy/<release>-ai-memory -c mcp-auth | grep jwks
```
- `/mcp` should answer 401 without a bearer (auth wired) and 200 with a valid JWT.
- `/web` should redirect to Keycloak login when oauth2-proxy is on.

## 5. Upgrade
Bump the chart version in the repo; in Rancher the new version appears under the
app → **Upgrade**, preserving the answered Questions.

## Rollback
Rancher app → **History → Rollback**, or `helm -n knowledge-center rollback <release>`.
