# memcli — memória pessoal (ai-memory) para agentes standalone

Wrapper de leitura/escrita da memória de longo prazo (`memory.djalmajr.dev`)
via HTTP puro — **sem MCP, sem hooks**. Fonte canônica: [`bin/memcli`](../bin/memcli).

Feito para agentes tipo OpenClaw/Grok bot com shell próprio: o agente descobre
o uso pelo próprio tool (`memcli help`); nenhuma URL/token vai no prompt.

## Instalação (no shell do bot)

```bash
# 1. Obter o script (o bot lê este repo; ou receba o arquivo direto)
install -m 755 bin/memcli ~/.local/bin/memcli   # ou qualquer dir no PATH do bot

# 2. Token — SÓ no ambiente do serviço do bot, nunca em repo/gist/prompt
#    (env do container, EnvironmentFile do systemd, config de env do openclaw…)
export AI_MEMORY_AUTH_TOKEN=<token>

# 3. Smoke
memcli projects
memcli search ai-memory "teste" 1
```

## Fiação nas instruções do bot

Uma linha no arquivo de instruções (AGENTS.md/TOOLS.md do workspace do bot):

```
Memória de longo prazo: comando `memcli` via shell (rode `memcli help` na primeira dúvida).
```

## Escrita

Desabilitada por padrão. Habilite só em bot confiável, no env do serviço:
`MEMCLI_ALLOW_WRITE=1`. A escrita usa `POST /admin/write-page` e **exige o
token root** — além disso o subcomando `write` depende do binário `ai-memory`
no PATH; sem ele, o bot fica somente-leitura (search/read/recent/projects,
que são `curl` puro).

## Modelo de ameaça (leia antes de dar o token a qualquer serviço)

O wrapper tira o token de prompts, transcripts e configs — é **higiene, não
fronteira de segurança**. Um agente com shell no mesmo ambiente do token
consegue lê-lo. Fronteira real = shell do bot em outro usuário/container com
credencial própria e escopada por consumidor — que é o plano da
[issue #9](https://github.com/djalmajr/ai-memory-ops/issues/9) (chaves por
consumidor no mcp-auth, bloqueada em `AI_MEMORY_AUTH__ACTOR_PROXY_BEARER_TOKEN`
ausente no compose). Até lá, o único token existente é o **root** (poder total
de escrita/admin): entregue-o apenas a bots que você controla integralmente.

## Limitações conhecidas

- Busca é FTS5 (léxica) — o retrieval híbrido (RRF fts+entity+vector) é
  exclusivo do `memory_query` via MCP. Prefira termos concretos.
- Sem hooks: nada é capturado automaticamente desse bot; memória é pull-only
  + escrita manual durável.
- Fallback de Keychain (`security`) só existe em macOS; em Linux/container o
  env é o único caminho.
