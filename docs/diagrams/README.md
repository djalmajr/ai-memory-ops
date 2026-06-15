# Architecture diagrams

Excalidraw diagrams of the **ai-memory-ops** architecture, ordered for a
first-time reader. Open any `.excalidraw` file at <https://excalidraw.com> (File
-> Open) or with the **Excalidraw** VS Code extension.

| # | File | What it answers |
|---|------|-----------------|
| 00 | [`00-overview.excalidraw`](00-overview.excalidraw) | The whole system at a glance: who calls it, what runs, what it talks to. **Start here.** |
| 01 | [`01-runtime-topology.excalidraw`](01-runtime-topology.excalidraw) | What actually runs in the cluster — the single pod, Service, PVC, webhooks, CronJobs. |
| 02 | [`02-auth-flows.excalidraw`](02-auth-flows.excalidraw) | How a request is authenticated — machine (JWT), browser (oauth2-proxy), hooks, RFC 9728 discovery. |
| 03 | [`03-keycloak-onboarding.excalidraw`](03-keycloak-onboarding.excalidraw) | The Keycloak realm (3 clients, roles, group) and the 2-step developer onboarding. |
| 04 | [`04-write-admission-data.excalidraw`](04-write-admission-data.excalidraw) | The write path + admission chain (scope-guard -> contributors -> git-mirror) and the ETL / capacity jobs. |
| 05 | [`05-build-release-repo-map.excalidraw`](05-build-release-repo-map.excalidraw) | CI that builds the 6 images, the Helm chart, the deploy path, and a map of this repo. |

## Conventions

- **Solid arrow** = primary request / data flow. **Dashed arrow** = secondary
  (discovery, config, async, read-only).
- Colour encodes role: blue = clients, violet = ingress/Service, orange =
  auth edge, green = engine, teal = storage, red = Keycloak/IdP, indigo =
  admission webhooks, amber = jobs, grey = external/config.
- Everything is **generic** — placeholders like `keycloak.example.com`,
  `<realm>`, `<owner>`. No instance-specific values live in this public repo
  (per-instance config lives in each deployment's own values file).

## Regenerating

The diagrams are generated from a single script so they stay consistent:

```sh
python3 docs/diagrams/_build.py
```

Edit the data tables in `_build.py` (nodes / edges / zones per diagram) and
re-run. You can also hand-edit a `.excalidraw` file in the editor and keep it;
the script overwrites, so fold manual changes back into `_build.py` if you want
them to survive a regen.
