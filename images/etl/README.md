# ai-memory-svc-etl image

Docker image of the ai-memory-svc Knowledge Center **`etl-cron` sidecar**.

`supercronic` (PID 1) triggers `run-etl.sh` on the schedule defined in `ETL_SCHEDULE`.
`run` mode (one-shot) is also available for manual execution via `kubectl exec` or
`docker run ... run`.

## Stack

| Component | Version | Source |
|---|---|---|
| Base runtime | `node:24-bookworm-slim` (Node 24 LTS, Debian glibc) | Docker Hub |
| `npm` / `npx` | bundled with Node 24 | — |
| `opencode` CLI | v1.15.4 glibc (linux-x64 / linux-arm64) | GitHub `anomalyco/opencode` |
| Skills `my-org/skills` | wiki-ingest, wiki-lint, wiki-init (installed at build) | `npx -y skills add -a opencode -g` |
| `supercronic` | v0.2.45 (PID 1, does native reaping) | Binary release `aptible/supercronic` |
| **Indexer (0.1.1+)** | `better-sqlite3` ^11, `sqlite-vec` ^0.1, `gray-matter` ^4, `glob` ^10 | npm |
| `git`, `curl`, `ca-certificates`, `jq`, `ripgrep`, `sqlite3` | apt from Debian Bookworm | — |

No `tini` (supercronic does the reaping). Simplified single-stage. No `node-llama-cpp` yet (planned for later — GGUF rerank).

**Approximate size: ~500-600 MB** (Debian glibc + Node + opencode + skills + node_modules).

## Build

```bash
docker build --platform linux/amd64 -t ai-memory-svc-etl:0.1.0-rcN images/etl/
docker images ai-memory-svc-etl | head -3
```

`--platform linux/amd64` is required on Apple Silicon Macs (lab and prod run amd64).

## Execution modes

### Default (cron)

`supercronic` reads `ETL_SCHEDULE`, waits for the time, triggers `run-etl.sh`:

```bash
docker run -d --name wiki-etl \
  -e ETL_SCHEDULE='*/5 * * * *' \
  -e GITLAB_API_TOKEN=... \
  -e OPENCODE_API_KEY=... \
  -e WIKI_REPO_URL=http://gitlab.example.com/my-org/ai-memory-content.git \
  -e WIKI_SOURCES_JSON='[{"name":"sample-project","repoUrl":"https://gitlab.example.com/my-org/sample-project.git","branch":"main"}]' \
  -v $(pwd)/etl-data:/data \
  ai-memory-svc-etl:0.1.0-rcN
```

### One-shot (run)

Useful for local testing and for on-demand `kubectl exec` inside the cluster:

```bash
docker run --rm \
  -e GITLAB_API_TOKEN="$GITLAB_SOURCE_TOKEN" \
  -e OPENCODE_API_KEY="$OPENCODE_API_KEY" \
  -e WIKI_REPO_URL=http://gitlab.example.com/my-org/ai-memory-content.git \
  -e WIKI_SOURCES_JSON='[{"name":"sample-project","repoUrl":"https://gitlab.example.com/my-org/sample-project.git","branch":"main"}]' \
  -e WIKI_LANGUAGE=pt-BR \
  -e OPENCODE_MODEL=kimi-k2.6 \
  -v $(pwd)/etl-data:/data \
  ai-memory-svc-etl:0.1.0-rcN run
```

## Run on the Multipass lab

The E2E execution happens **on the K3s lab** via `kubectl exec` in the `etl-cron` sidecar.
See [`runbooks/lab-deploy.md`](../../runbooks/lab-deploy.md) for the full flow
(DNS, CA, registry, push, helm install, exec, validation).

Shortcuts:

```bash
# Build (Mac, amd64 platform)
docker build --platform linux/amd64 -t ai-memory-svc-etl:0.1.0-rc1 images/etl/

# Push to the lab internal registry (registry.example.com, GitLab Container Registry, TLS via your-ca-issuer)
docker tag ai-memory-svc-etl:0.1.0-rc1 registry.example.com/my-org/ai-memory-svc-etl:0.1.0
docker push registry.example.com/my-org/ai-memory-svc-etl:0.1.0

# Helm install on the lab
helm upgrade --install ai-memory-svc charts/ai-memory-svc \
  --kubeconfig ~/.kube/config \
  -n ai-memory -f charts/ai-memory-svc/values-lab.yaml \
  --rollback-on-failure --wait --timeout 5m

# Trigger an E2E execution manually
POD=$(kubectl --kubeconfig ~/.kube/config -n ai-memory \
  get pod -l app.kubernetes.io/name=ai-memory-svc -o jsonpath='{.items[0].metadata.name}')
kubectl --kubeconfig ~/.kube/config -n ai-memory \
  exec -it "$POD" -c etl-cron -- /usr/local/bin/entrypoint.sh run
```

## Consumed env vars

| Env var | Source (chart) | Required? |
|---|---|---|
| `GITLAB_API_TOKEN` | Secret `ai-memory-svc-secrets` | yes |
| `OPENCODE_API_KEY` | Secret `ai-memory-svc-secrets` | yes |
| `MCP_AUTH_TOKEN` | Secret `ai-memory-svc-secrets` | no (consumed only by the MCP) |
| `WIKI_REPO_URL` | ConfigMap `ai-memory-svc-config` | yes |
| `WIKI_REPO_BRANCH` | ConfigMap `ai-memory-svc-config` | no (default `main`) |
| `WIKI_SOURCES_JSON` | ConfigMap `ai-memory-svc-config` | yes |
| `WIKI_PATH` | ConfigMap `ai-memory-svc-config` | no (default `/data/wiki-content`) |
| `WIKI_DB_PATH` | ConfigMap `ai-memory-svc-config` | no (reserved for the indexer) |
| `WIKI_LANGUAGE` | ConfigMap `ai-memory-svc-config` | no (default `pt-BR`) |
| `OPENCODE_MODEL` | ConfigMap `ai-memory-svc-config` | no (default `kimi-k2.6`) |
| `MODELS_PATH` | ConfigMap `ai-memory-svc-config` | no (reserved for the indexer) |
| `ETL_SCHEDULE` | ConfigMap `ai-memory-svc-config` | no (default `0 0 * * *`) |
| `GIT_AUTHOR_NAME` / `GIT_AUTHOR_EMAIL` | ConfigMap `ai-memory-svc-config` | no (default bot) |
| `GIT_COMMITTER_NAME` / `GIT_COMMITTER_EMAIL` | ConfigMap `ai-memory-svc-config` | no (default bot) |
| `SKILLS_UPDATE_ON_RUN` | hardcoded `false` (override via env) | no — `true` updates skills before the run |
| `ETL_RUN_INDEXER` | Deployment (from `etl.runIndexer` in values.yaml) | no — `true` (default) runs the indexer at the end of the ETL |
| `DB_PATH` | ConfigMap `ai-memory-svc-config` | no (default `/data/db/wiki.sqlite`) |


## Files embedded in the image

- `/usr/local/bin/entrypoint.sh` — chooses between `cron` and `run`.
- `/usr/local/bin/run-etl.sh` — the actual orchestrator (clone + ingest + lint + commit + push).
- `/etc/supercronic.tmpl` — crontab template; `entrypoint.sh` replaces `@@SCHEDULE@@`.
- `/usr/local/share/wiki-ingest/ingest-instructions.md` — prompt guidelines for the `wiki-ingest` skill. Referenced by `run-etl.sh` via the `INGEST_INSTRUCTIONS` env.
- `/root/.agents/skills/wiki-ingest/`, `wiki-lint/`, `wiki-init/` — bundled skills (installed at build via `npx -y skills add my-org/skills -a opencode -g`).

## Smoke tests

After build, validate the stack:

```bash
# Stack versions
docker run --rm ai-memory-svc-etl:0.1.0-rcN supercronic --version
docker run --rm ai-memory-svc-etl:0.1.0-rcN opencode --version
docker run --rm ai-memory-svc-etl:0.1.0-rcN git --version
docker run --rm ai-memory-svc-etl:0.1.0-rcN node --version

# Installed skills
docker run --rm ai-memory-svc-etl:0.1.0-rcN ls /root/.agents/skills/

# Script syntax (does not execute)
docker run --rm --entrypoint bash ai-memory-svc-etl:0.1.0-rcN -n /usr/local/bin/run-etl.sh
docker run --rm --entrypoint bash ai-memory-svc-etl:0.1.0-rcN -n /usr/local/bin/entrypoint.sh

# embedded ingest-instructions.md
docker run --rm ai-memory-svc-etl:0.1.0-rcN head -5 /usr/local/share/wiki-ingest/ingest-instructions.md
```

## Push to registries

### Lab (`registry.example.com`, GitLab Container Registry, TLS via your-ca-issuer)

```bash
docker tag ai-memory-svc-etl:0.1.0-rcN registry.example.com/my-org/ai-memory-svc-etl:0.1.0-rcN
docker push registry.example.com/my-org/ai-memory-svc-etl:0.1.0-rcN
```

Auth integrated with GitLab (root + password, or PAT). TLS works because the `your-ca-issuer` self-signed CA was imported into `~/.docker/certs.d/registry.example.com/ca.crt`.

### Prod (Nexus)

See [`runbooks/nexus-image-flow.md`](../../runbooks/nexus-image-flow.md) — credentials in `<edge-runtime>/.env`.

```bash
# Summarized (see the full runbook):
source <edge-runtime>/.env
echo "$NEXUS_PASS" | docker login "$NEXUS_URL" -u "$NEXUS_USER" --password-stdin
docker tag ai-memory-svc-etl:0.1.0 "$NEXUS_URL/ai-memory-svc-etl:0.1.0"
docker push "$NEXUS_URL/ai-memory-svc-etl:0.1.0"
```

The stable `0.1.0` tag is only promoted once the E2E ingest on the lab passes.

## Troubleshooting

### "skill X not found"
Check that `npx -y skills add my-org/skills -a opencode -s X -g` is in the Dockerfile (install step). Rebuild with `--no-cache` if in doubt.

### "opencode: auth failed" / "OPENCODE_API_KEY invalid"
Check `OPENCODE_API_KEY` in the env. In the cluster, it comes from the `ai-memory-svc-secrets` Secret. Local: from the operator's shell (`$OPENCODE_API_KEY`).

### "supercronic does not trigger"
- `ETL_SCHEDULE` must be cron 5-field (not Quartz 6-field).
- The container logs show `[entrypoint] supercronic schedule = ...`. If empty, the env var was not read.
- Force a manual run: `docker exec <container> /usr/local/bin/run-etl.sh` or `kubectl exec ... -- /usr/local/bin/entrypoint.sh run`.

### "git push 401" / "git clone 401"
`GITLAB_API_TOKEN` must have the `read_repository` scope for clones and `write_repository` for the push to `wiki-content`. The token comes from the `ai-memory-svc-secrets` Secret in the cluster.

### "x509: certificate signed by unknown authority" on push to the lab
Docker Desktop did not reload the CA. Confirm that `~/.docker/certs.d/registry.example.com/ca.crt` exists and restart Docker Desktop.

### Image too large (> 800 MB)
- Check that `apt-get install` has `--no-install-recommends` and `rm -rf /var/lib/apt/lists/*`.
- Check that `npm cache` was not inflated (the current build does not need it, it only uses `npx`).

## Changes between tags

- `0.1.0-rc0` — stack installed, skeleton `run-etl.sh` (only validates env vars).
- `0.1.0-rc1+` — actual body of `run-etl.sh` + embedded `ingest-instructions.md`.
- `0.1.0` — stable tag when the E2E on the lab passes and the prompt is good.
