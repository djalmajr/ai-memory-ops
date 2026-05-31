# Runbooks

Operational runbooks for deploying and running **ai-memory-svc**. These are
step-by-step procedures for a human operator (or an agent acting as one).

| # | Runbook | When |
|---|---------|------|
| 01 | [Install on Rancher via Questions](01-serpro-rancher-install.md) | First install / upgrade on a client Rancher (e.g. Serpro), done manually through the Helm Charts UI |
| 02 | [Secrets bootstrap](02-secrets-bootstrap.md) | Before the first install — the chart never templates real credentials |
| 03 | [Ingress auth: Traefik vs nginx](03-ingress-auth-traefik-vs-nginx.md) | Understand how the chart auto-picks the ingress backend and wires edge auth on each |

> The canonical, always-current knowledge also lives in the **ai-memory wiki**
> (`notes/serpro-rancher-deploy.md` and `runbooks/*`). The repo copies are the
> versioned source; the wiki is the queryable mirror.
