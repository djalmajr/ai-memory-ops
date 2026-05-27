# ETL guidelines — `wiki-ingest` for my-org projects

> This file is embedded in the `ai-memory-svc-etl` image at
> `/usr/local/share/wiki-ingest/ingest-instructions.md`. `run-etl.sh` references
> this path in the prompt sent to `opencode` when triggering the `wiki-ingest` skill.

## Objective

Generate and keep up to date a **technical wiki** in Markdown for each my-org
project, in **pt-BR**, with a predictable structure and coherent code citations.
The wiki lives in the `my-org/ai-memory/wiki-content` repo, under
`wiki/<project>/`.

## Language and tone

- **Language:** `pt-BR` (Brazilian Portuguese), correct grammar and spelling.
- **Tone:** technical, concise, neutral. No marketing, no opinion. No emojis.
- **Audience:** developers and architects who will consult the wiki to
  answer quickly: *"what it is, how it works, where it is, what it talks to"*.

## Output structure

For each project under `wiki/<project>/`, generate **at minimum**:

```
wiki/<project>/
├── index.md                    # Entry point: what it is, status, internal links
├── architecture.md             # Architectural view (layers, runtime, deploy)
├── modules/
│   ├── <module-a>.md           # One per relevant module/service/top-level folder
│   └── <module-b>.md
├── dependencies.md             # External libs, versions, purpose
└── history/
    └── events.jsonl            # Append-only — filled by the ETL, do NOT edit via skill
```

Optional files (create if there is content):

- `runbooks/<topic>.md` — operational procedures the code suggests.
- `decisions/ADR-NNNN-<slug>.md` — *only if* there is a clear ADR in the code/docs.
- `glossary.md` — when there is dense domain jargon.

## Required frontmatter

Every `*.md` (except `history/events.jsonl`) must start with:

```yaml
---
title: "<short human title>"
slug: <slug-kebab>
project: <project>
kind: index | architecture | module | dependencies | runbook | adr | glossary
status: draft                  # draft | reviewed | stale
audience: dev                  # dev | mixed | ops | business
updated: <YYYY-MM-DD>
sources:
  - path: <path-in-source>
    ref: <short-commit-hash>
tags: [<tag>, <tag>]
---
```

- The `sources` block is **required in EVERY `*.md`** (`wiki-lint` is blocking and requires a non-empty `sources` even on index pages). For index pages (`kind: index`, including `modules/index.md`), point `sources` to the directory the index covers (e.g.: `path: src` or `path: src/modules`), with the `ref` of the ingested commit. Do **not** omit `sources` on index pages — that causes `etl-failed` in wiki-lint.
- The `audience` field is **required**: use `dev` for technical files (architecture, modules, dependencies, runbook, adr); `mixed` only on `index.md`; `business`/`ops` for specific files when applicable.

## CONVENTIONS.md (generate once on the first run)

Before generating the project files, check whether `wiki/CONVENTIONS.md` exists at the root of `wiki-content`. If it does **not** exist, create it with this content (only once — do not regenerate on subsequent runs):

```markdown
---
title: "Wiki Conventions"
slug: conventions
kind: meta
status: reviewed
audience: mixed
updated: <YYYY-MM-DD>
tags: [meta, conventions]
---

# Wiki Conventions

## Frontmatter

Every `wiki/<project>/**/*.md` file must have YAML frontmatter with the required
fields: `title`, `slug`, `project`, `kind`, `status`, `audience`, `updated`,
`tags`, and `sources` (when it describes code).

## Linking

- Internal links: relative path within the same project (`./modules/auth.md`)
  or absolute path from the wiki root (`/wiki/<other-project>/index.md`).
- External links: HTTPS when possible; never include tokens/secrets in URLs.

## Naming

- Files: `kebab-case.md`. Folders: `kebab-case/`.
- No spaces in names; no special characters other than `-`.

## Language

`pt-BR` for all content, per `.wiki-guardrails.yml`. Proper nouns,
APIs and code identifiers remain in the original.

## Updating

ETL re-runs overwrite the content. To preserve manual changes,
open an MR on `wiki-content`. History stays in Git.
```

## Citation rules

- When citing code, prefer **relative path + function/class name** instead of
  pasting large snippets. E.g.: `src/auth/login.ts:authenticate()`.
- When pasting code, limit to ≤ 15 lines and use a block with the language name.
- Never invent paths, names or behaviors that do not exist in the source.

## Scan limits

Ignore the following paths in the source:

- `node_modules/`, `vendor/`, `dist/`, `build/`, `out/`, `coverage/`
- `.git/`, `.github/`, `.gitlab/` (except when describing CI/CD in `architecture.md`)
- `*.lock`, `*.lock.json`, `package-lock.json`, `yarn.lock`, `pnpm-lock.yaml`
- Binary files: `*.png`, `*.jpg`, `*.zip`, `*.pdf`, `*.woff*`

## What NOT to write in the wiki

- **Do not invent** features. If something is uncertain, mark it with `status: draft`.
- **Do not duplicate** the project README. Replace, do not repeat.
- **Do not include** secrets, tokens, passwords, internal URLs with credentials.
- **Do not make value judgments** about the code ("this could be better"). Describe.

## Idempotency

- Re-runs **overwrite** the generated markdown. Git is the history.
- If a file's content does not change between runs, **do not rewrite it** (avoids
  noisy commits). The `wiki-ingest` skill must respect this.

## Specific case — `sample-project`

Pilot project. Typical structure expected from the source:

- `src/` or equivalent folders with the product modules.
- Dependencies in `package.json` (Node) or `pyproject.toml`/`requirements.txt` (Python).
- Configuration in `config/`, `env`, or environment variables.

**Specific focus of `sample-project`:**

- Map modules (assets, sensors, events, billing, integrations) when applicable.
- Document external contracts: APIs the project consumes or exposes.
- Technical decisions visible in comments or ADRs of the source.

## Controlled failure

If the source is empty or there is no analyzable code, the skill must:

1. Generate `wiki/<project>/index.md` with only frontmatter + a note "Empty source on <date>".
2. Not create other files.
3. Exit with exit 0 (the ETL treats it as an empty success).
