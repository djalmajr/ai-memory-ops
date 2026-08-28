# memcli — standalone ai-memory client for AI agents

A read-focused wrapper over the personal ai-memory server
(`memory.djalmajr.dev`) using plain HTTP — **no MCP, no hooks**. Canonical
source: [`bin/memcli`](../bin/memcli).

Built for OpenClaw-style bots with their own shell: the agent discovers usage
from the tool itself (`memcli help`); no URL or token goes into the prompt.

memcli **mirrors the officially documented surface** of upstream
[ai-memory](https://github.com/akitaonrails/ai-memory) — the `/api/v1`
endpoint table in the README, `docs/frontend-api.md`, `docs/install.md`, and
`docs/usage.md`. When in doubt, the official docs win.

## Install (inside the bot's shell)

```bash
# 1. Get the script (the bot can read this repo; or hand it the file directly)
install -m 755 bin/memcli ~/.local/bin/memcli   # any dir on the bot's PATH

# 2. Token — ONLY in the bot service's environment, never in a repo/gist/prompt
#    (container env, systemd EnvironmentFile, the runtime's per-agent env block…)
export AI_MEMORY_AUTH_TOKEN=<token>

# 3. Smoke test
memcli projects
memcli search ai-memory "test" 1
```

## Wiring into the bot's instructions

One line in the bot's workspace instruction file (AGENTS.md/TOOLS.md):

```
Long-term memory: `memcli` via shell (run `memcli help` on first doubt).
```

## Authentication — one key, two ways to use it

The same `AI_MEMORY_AUTH_TOKEN` bearer works for every access form:

1. **Raw URL/HTTP** (what memcli does):
   `curl -H "Authorization: Bearer $AI_MEMORY_AUTH_TOKEN" https://memory.djalmajr.dev/api/v1/...`
2. **The `ai-memory` CLI** (thin-client commands): env
   `AI_MEMORY_AUTH_TOKEN` + `AI_MEMORY_SERVER_URL`, or `config.toml`
   (`server_url` + `[auth] bearer_token`). **Env always overrides
   config.toml** — a stale env token yields 401 even with a correct config.

## Command surface

Read (pure curl): `workspaces`, `projects`, `graph`, `search`, `read`,
`pages`, `recent`, `briefing`, `overview`, `handoffs` (read-only),
`sessions`, `observations`, `status` (HTTP ping fallback).

Mutation (delegates to the `ai-memory` binary, gated by env):
`write` (`MEMCLI_ALLOW_WRITE=1`), `delete` (`MEMCLI_ALLOW_DELETE=1`).
Both use root-token admin endpoints; without the binary the bot is
read-only by construction.

## The full `ai-memory` CLI — broader operator surface

memcli is **not required and does not restrict the bot** — an agent with a
shell plus the root token can use raw curl or the full CLI anyway, so hiding
verbs is not security. The official CLI covers the broader operator surface
(delete-page, status, backup, maintenance, administration). Official prebuilt
binaries:

- <https://github.com/akitaonrails/ai-memory/releases/latest> —
  `ai-memory-<os>-<arch>.tar.gz` with `.sha256` sidecars
- Arch Linux: `yay -S ai-memory-bin`
- `cargo install ai-memory` does **not** exist (crate name taken)

Suggested instruction when giving a bot the full CLI:

```
Memory: `ai-memory` CLI (env AI_MEMORY_SERVER_URL + AI_MEMORY_AUTH_TOKEN).
ALWAYS pass explicit --workspace and --project — you run standalone; never
trust cwd auto-derivation. NEVER use destructive/admin subcommands
(purge-project, reset, restore, move-*, user) without an explicit user order.
```

That is instruction (soft), not containment: destructive verbs stay one
`--confirm` away.

## Limits — no single non-MCP interface is complete today

- Hybrid `memory_query` (RRF fts+entity+vector) is MCP-only; memcli/CLI/REST
  search is lexical FTS5, measurably weaker. Prefer concrete terms.
- Creating/accepting handoffs, `memory_feedback`, `memory_explore`, and
  consolidation are MCP-only.
- Without hooks, nothing is captured automatically from the bot; memory is
  pull-only plus manual durable writes.

## Threat model (read before giving the token to any service)

The wrapper keeps the token out of prompts, transcripts, and configs — that
is **hygiene, not a security boundary**. An agent with a shell in the same
environment as the token can read it. A real boundary means the bot's shell
runs under another user/container with its own per-consumer scoped
credential — the plan in
[issue #9](https://github.com/djalmajr/ai-memory-ops/issues/9) (per-consumer
keys in mcp-auth, currently blocked on the missing
`AI_MEMORY_AUTH__ACTOR_PROXY_BEARER_TOKEN` in the compose). Until then the
only existing token is **root** (full write/admin power): hand it only to
bots you fully control.

## Known limitations

- The macOS Keychain fallback (`security`) only exists on macOS; in
  Linux/containers the environment variable is the only path.
- `MEMCLI_SERVER_URL` and `MEMCLI_WORKSPACE` override the defaults for other
  deployments.
