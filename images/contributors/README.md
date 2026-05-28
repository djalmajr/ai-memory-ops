# contributors

Admission webhook for [ai-memory](https://github.com/akitaonrails/ai-memory) — first
concrete extension built on the admission webhook chain primitive. Go stdlib
implementation (no external deps).

## What it does

For each page write that flows through `Wiki::write_page`, the engine POSTs the
page + actor context to this service. It appends the actor (agent, user, sub,
client) to `frontmatter.contributors` as an **append-only set keyed by
`(agent, client)`** — repeat writes from the same agent install bump
`last_seen` + `writes`; new agents add new entries.

```yaml
contributors:
  - agent: claude-code
    user: djalmajr
    sub: 8f3a-...
    client: 72836f52-...
    first_seen: 2026-05-28T03:14:00Z
    last_seen:  2026-05-28T07:22:00Z
    writes: 3
  - agent: codex
    user: djalmajr
    sub: 8f3a-...
    client: 9aef1230-...
    first_seen: 2026-05-28T05:00:00Z
    last_seen:  2026-05-28T05:00:00Z
    writes: 1
```

## HTTP contract

```
POST /enrich
Content-Type: application/json
X-Memory-Op: write_page | consolidate

{
  "page":  { "path": "gotchas/x.md", "frontmatter": {...}, "body": "..." },
  "ctx":   { "actor": { "agent": "claude-code", "user": "...", "sub": "...",
                        "client": "...", "session_id": "..." }, ... }
}

→ 200 OK  { "page": { "frontmatter": <mutated> } }   when actor has agent+client
→ 204 No Content                                      when actor is empty (anonymous)
```

`GET /healthz` → `200 ok` (liveness/readiness probe).

## Config

| Env | Default | Meaning |
|---|---|---|
| `LISTEN_ADDR` | `0.0.0.0:8080` | bind address |
| `LOG_LEVEL`   | `info`         | `debug` \| `info` \| `warn` \| `error` |

## Wiring

In the engine's `config.toml`:

```toml
[[admission_webhooks]]
name = "contributors"
url  = "http://contributors.<namespace>.svc.cluster.local:8080/enrich"
timeout_ms = 2000
failure_policy = "ignore"
events = ["write_page", "consolidate"]
```

Or via Helm chart `ai-memory-svc` (auto-injects the URL when
`webhooks.contributors.enabled=true`).

## Development

```bash
go run .
# (in another shell)
curl -s -XPOST http://127.0.0.1:8080/enrich \
  -H 'Content-Type: application/json' \
  -d '{"page":{"path":"x.md","frontmatter":null,"body":""},"ctx":{"actor":{"agent":"claude-code","client":"c1"}}}' \
  | jq
```

## Tests

```bash
go test ./...
```

Covers: anonymous-actor 204, new-contributor append, repeat-write increment,
distinct-clients distinct entries.
