# Upgrade `memory.djalmajr.dev`: engine 1.37.0 (fork `9f1762ad`) → v2.0.2

**Status:** CONCLUÍDO 2026-09-04 — engine 2.0.2 em produção. Desvio: migrations do fork renumeradas no upstream exigiram correção cirúrgica do `refinery_schema_history` (ver registro `infra/registros/2026-09-04-upgrade-2.0.2-orcarouter-penpot`).

## Contexto

- Instância pessoal em Hetzner (`167.235.206.217`), Docker Compose em `/opt/ai-memory`.
- Engine em produção: imagem `ghcr.io/djalmajr/ai-memory-ops/ai-memory@sha256:8b78527b…`,
  buildada do fork `djalmajr/ai-memory` no commit `9f1762ad` (= v1.37.0 + 1 commit
  `feat(auth): add browser sessions and API credentials`, branch `feat/admin-console`).
  Esse commit foi mergeado upstream como PR #533 em 2026-09-01, **antes** da v2.0.0.
  Diff de rotas `9f1762ad` → `v2.0.2`: nenhuma rota removida, 14 novas (`/admin/compact`,
  `/admin/export-okf`, `/admin/human-users`, `/admin/users/{u}/{expire,revive,rotate-token}`…).
  A UI custom (`djalmajr/ai-memory-ui@aa3c367`) só consome rotas presentes na v2.0.2.
- Dados: `/opt/ai-memory/data` = 7,8 GB (db + wiki + logs). Disco: 262 GB livres.
- Config atual já tem `AI_MEMORY_EMBEDDING_PROVIDER=openai` (Cloudflare bge-m3) → a 2.0
  **não** liga embeddings locais. `AI_MEMORY_AUTO_SCOPE__MODE=per_actor` já é o novo default.
  LLM: OrcaRouter `z-ai/glm-5.3-flash` (trocado hoje).
- Todas as chaves de config em uso (`auth.root_username/root_subject/secure_cookie/
  trusted_proxy_cidrs`, `routing.mid_session`, `decay.hard_delete_after_days`,
  `admission_webhooks`, `auto_scope`, `allowed_hosts`) existem no `config.rs` da v2.0.2.
- Fork `djalmajr/ai-memory`: `main` = 0 à frente / 208 atrás do upstream; **sem tag v2.0.2**.
  O Dockerfile do ops faz cache-bust via `api.github.com/repos/djalmajr/ai-memory/commits/<ref>`
  → a tag precisa existir no fork.
- Nossa imagem **não** define `AI_MEMORY_IN_CONTAINER`, mas a 2.0.2 também detecta
  `/.dockerenv` → backup pré-migração vai para `/data/backups/` (volume persistente).
- CLI local (`~/.local/bin/ai-memory`) em 1.32.2; hooks Claude/Codex/Cursor/Gemini/OpenCode
  apontam para o servidor.

## O que a 2.0 faz no primeiro start (irreversível)

1. Gera `/data/backups/ai-memory-backup-okf-v0.2-<data>.tar.gz` (≈ tamanho do data dir),
   reabre e verifica; aborta se falhar.
2. Reescreve todas as páginas do wiki em OKF v0.2 (frontmatter `type/generated/sources/
   stale_after`, `okf_version` por projeto). IDs/versões/timestamps preservados.
3. Migrations de schema V50 → V58 (tombstone `purged_sessions`, janelas de validade de
   entidades, FTS de tags) sobre 5,2 M observações — esperar minutos de indisponibilidade.
4. Binário 1.x deixa de poder escrever no data dir. Rollback = restaurar tarball + imagem antiga.

## Arquivos / artefatos

| Onde | O quê |
|---|---|
| `djalmajr/ai-memory` (fork) | sincronizar `main` com `upstream/main`; push da tag `v2.0.2` |
| `djalmajr/ai-memory-ops` → `build-images.yml` | dispatch `image=ai-memory engine_ref=v2.0.2 ui_ref=aa3c367eabe5dc8479aa0628fc37af89e6f1d8a5` |
| host `/opt/ai-memory/compose.yml` | trocar digest da imagem `ai-memory`; adicionar `AI_MEMORY_IN_CONTAINER: "1"` (explícito) |
| host `/opt/ai-memory/` | backups `compose.yml.bak-v2-<ts>`, `.env.bak-v2-<ts>`, `ai-memory backup` manual antes |
| laptop `~/.local/bin/ai-memory` | 2.0.2 macos-aarch64 do release upstream (sha256), backup `ai-memory.bak-1.32.2` |
| laptop hooks | `ai-memory install-hooks --agent <claude-code,codex,…> --apply` |

## Tasks

- [x] 1. Fork: `git push origin upstream/main:main` (FF) e `git push origin v2.0.2`.
      Cancelar o `release.yml` do fork que a tag dispara (release oficial upstream já tem binários).
- [x] 2. Build: `gh workflow run build-images.yml -R djalmajr/ai-memory-ops -f image=ai-memory
      -f engine_ref=v2.0.2 -f ui_ref=aa3c367eabe5dc8479aa0628fc37af89e6f1d8a5`; aguardar; anotar digest.
- [x] 3. Verificar imagem antes do deploy: `docker run --rm --entrypoint ai-memory <img@digest> --version` = `2.0.2`.
- [x] 4. Host — rede de segurança fora do volume: `docker exec ai-memory-ai-memory-1 ai-memory backup
      --to /data/backup-pre-v2-<ts>.tar.gz` e copiar para `/opt/ai-memory/` (fora de `data/`).
      Conferir `df` (precisa de ~2× 8 GB livres; há 262 GB).
- [x] 5. Host — `compose.yml`: novo digest + `AI_MEMORY_IN_CONTAINER: "1"`; backups `.bak-v2-<ts>`.
- [x] 6. Host — `docker compose up -d ai-memory`; acompanhar `docker compose logs -f ai-memory`
      até ver backup verificado, migração OKF concluída, migrations V51–V58 aplicadas e
      `ai-memory starting version="2.0.2"`.
- [x] 7. Validar: `/mcp` 401, UI logada (`/login`, `/admin/status`, `/admin/users`,
      `/admin/api-credentials`), `memory_status`, `memory_query` com vetor (embedder Cloudflare ativo),
      `memory_explore` (LLM OrcaRouter), git-mirror `push ok` após uma escrita de teste.
- [x] 8. Laptop: baixar CLI 2.0.2 upstream, sha256, substituir com backup; `ai-memory --version`;
      `install-hooks --apply` por agente; abrir sessão nova e confirmar captura (spool drena, sem 401/403).
- [x] 9. Registrar no ai-memory (`infra` → `registros/2026-09-04-upgrade-2.0.2.md`) e atualizar
      o runbook 10 (imagem, digest, data, OKF).

## Riscos e mitigação

- **git-mirror desatualizado:** a migração OKF reescreve arquivos sem passar pela admission chain;
  o espelho `ai-memory-bkp` só volta a convergir nas próximas escritas. Aceitável; opcionalmente
  disparar um sync manual depois.
- **Payloads da UI custom:** paths iguais, mas #533 pode ter sido ajustado no merge. Mitigação:
  smoke manual da UI (task 7) antes de declarar sucesso; rollback é um `compose up` com o digest antigo
  + restauração do tarball.
- **Duração da migração:** 5,2 M observações; se passar de ~15 min, não interromper — checar logs.
- **CLI 1.32.2 contra servidor 2.0.2:** compatível para hooks (`/hook/batch` inalterado), mas
  `install-hooks`/skills geradas mudaram; atualizar CLI no mesmo dia.

## Verificação (critério de pronto)

- `ai-memory --version` no container = `2.0.2`; log sem `error` na migração.
- `/data/backups/ai-memory-backup-okf-v0.2-*.tar.gz` presente e cópia externa em `/opt/ai-memory/`.
- `memory_status`, `memory_query`, `memory_explore` respondendo via MCP a partir do laptop com CLI 2.0.2.
- UI: login, projeto, página, admin/status carregam.

## Rollback

1. `docker compose stop ai-memory`.
2. Restaurar `compose.yml.bak-v2-<ts>` (digest antigo).
3. `rm -rf data/* && tar -xzf backup-pre-v2-<ts>.tar.gz -C data` (ou `ai-memory restore --from … --force`).
4. `docker compose up -d ai-memory`.
