# git-mirror

Admission webhook for [ai-memory](https://github.com/akitaonrails/ai-memory)
that mirrors each page write into an external git repo (e.g. a private GitLab
backup repo). Stdlib Go, ships `git` + `openssh-client` at runtime.

## Behaviour

1. Engine's admission chain POSTs `{ page, ctx }` to `/sync`.
2. Webhook renders `wiki/<workspace>/<project>/<page-path>` into the local
   working tree and `git commit`s it under a serialized mutex.
3. Returns `204` immediately — the engine is not blocked on the remote.
4. A background loop debounces commits and issues `git push` in batches.
   Failed pushes are retried with a cool-off until the remote is reachable.

## HTTP contract

```
POST /sync
Content-Type: application/json

{ "page": { "path": "...", "frontmatter": {...}, "body": "..." },
  "ctx":  { "workspace": "default", "project": "wiki-service",
            "actor": { "agent": "claude-code", "user": "djalmajr" } } }

→ 204 No Content   (write enqueued; failures logged but not surfaced when
                    used with failure_policy=ignore in the chain)
```

`GET /healthz` → `200 ok`.

## Config (env)

| Env | Default | Meaning |
|---|---|---|
| `REPO_URL`       | required               | SSH or HTTPS git remote URL |
| `REPO_BRANCH`    | `main`                 | branch to push to |
| `WORK_DIR`       | `/work/repo`           | local working tree |
| `GIT_USER`       | `ai-memory git-mirror` | commit author name |
| `GIT_EMAIL`      | `git-mirror@ai-memory.local` | commit author email |
| `PUSH_DEBOUNCE`  | `10s`                  | wait this long for commits to coalesce before pushing |
| `LISTEN_ADDR`    | `0.0.0.0:8080`         | bind address |
| `LOG_LEVEL`      | `info`                 | `debug` \| `info` \| `warn` \| `error` |

For SSH push, provide the deploy key via secret + `GIT_SSH_COMMAND` env
(see the chart template for the wiring).

## Tests

```bash
go test ./...
```

Covers: end-to-end commit against a local bare repo, path-traversal
rejection, frontmatter+body rendering.
