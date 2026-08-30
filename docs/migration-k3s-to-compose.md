# Migração: memory.djalmajr.dev de k3s para docker-compose

Objetivo: `memory.djalmajr.dev` rodando em docker-compose atrás do túnel
Cloudflare existente, com API keys por serviço para agentes e Cloudflare Access
para o navegador. Ao final, o lab (gitlab, registry, kas, rancher, minio,
minio-api, keycloak, tenancit, tenancit-id) e o próprio k3s saem.

Toda fase é verificável e reversível. O lab só cai depois do memory estar
provado fora dele.

## Estado atual (verificado em 2026-07-31, não presumido)

Host `167.235.206.217`, k3s. `cloudflared` roda como systemd **no host** e já
roteia 10 hostnames para `https://localhost:443`, incluindo o memory
(CNAME → `cfargotunnel`, proxied).

⚠️ A porta 443 pública está **aberta ao mundo** e responde direto com o `Host`
certo — hoje a Cloudflare é contornável. Três hostnames (`minio-api`,
`tenancit`, `tenancit-id`) são registros A `proxied=false` apontando para o IP,
e são eles que tornam a origem descobrível.

Cadeia do navegador hoje: CF → ingress → **oauth2-proxy** (valida Keycloak,
**injeta o `Authorization: Bearer` do engine**) → engine. É essa injeção que
faz a web UI funcionar sem você digitar chave — e é a peça que precisa de
substituto.

### Componentes a migrar

| Serviço | Imagem | Papel |
|---|---|---|
| engine | `ai-memory@sha256:d4351ed8…` (v1.24.0) | o servidor |
| git-mirror | `git-mirror:v1.13.0` | espelha o wiki para `github.com/djalmajr/ai-memory-bkp` (**não** para o GitLab local — removê-lo não afeta o backup) |
| scope-guard | `scope-guard:v0.12.0` | admission webhook, `ACL_RULES` |
| contributors | `contributors:latest` | admission webhook |
| mcp-auth | `mcp-auth:v1.24.0` | OAuth do MCP + tradução de token — **candidato a sair** junto com o Keycloak |

Segredos hoje em `memory-v2-wiki-service-secrets`: `AI_MEMORY_AUTH_TOKEN`,
`AI_MEMORY_AUTO_IMPROVE__SCHEDULER__ENABLED`, `HOOK_AUTH_TOKEN`, `LLM_API_KEY`,
`OPENAI_API_KEY`. Chave SSH do git-mirror em `memory-v2-git-mirror-ssh`.
Dados em PVC `local-path` (`memory-v2-wiki-service-data`, 10Gi) — já é um
diretório no host, o que torna a migração uma cópia.

## Fases

### Fase 1 — compose no ar, em paralelo, sobre CÓPIA dos dados

⚠️ **Dois processos ai-memory não podem escrever o mesmo SQLite/wiki.** A
validação em paralelo roda sobre uma **cópia**, nunca sobre o diretório vivo.

1. `ai-memory backup` no pod → tarball para fora do host (rede de segurança).
2. `/opt/ai-memory/` no host: `compose.yml`, `.env` (segredos), chave SSH.
3. Copiar o PVC para `/opt/ai-memory/data-probe/`.
4. Subir escutando **só em `127.0.0.1:49375`**, sobre a cópia.
5. Validar: `/mcp` responde, contagens batem com o pod, admission webhooks
   respondem, git-mirror **em modo dry-run** (não empurrar do clone!).

Reversível: `docker compose down`, apagar o diretório. Produção intocada.

### Fase 2 — cortar o túnel para o compose

1. Parar o Deployment do k3s (`scale --replicas=0`) — encerra as escritas.
2. `rsync` final do PVC para `/opt/ai-memory/data/`.
3. Subir o compose sobre os dados reais, em `127.0.0.1:49374`.
4. Mudar a regra de ingress do túnel: `memory.djalmajr.dev` →
   `http://localhost:49374` (some a terminação TLS local; a Cloudflare já faz
   TLS na borda).
5. Validar de fora.

Rollback: apontar a regra de volta para `https://localhost:443` e
`scale --replicas=1`. Segundos, uma linha.

**A exposição da origem morre aqui, por construção**: o container escuta em
loopback, então o túnel passa a ser o único caminho. Não é preciso allowlist
por IP nem mexer na 443 dos outros hostnames.

### Fase 3 — API keys por serviço

`ai-memory user add <serviço>` para cada consumidor permanente; o token é
impresso **uma vez** (só o hash fica no banco). Cada um vai para o `~/.zshenv`
do respectivo consumidor. Revogação individual por
`POST /admin/users/{u}/expire`; rotação por `.../rotate-token`.

Substitui o bearer único compartilhado. O token root continua existindo como
chave de administração e break-glass.

### Fase 4 — sessão humana no navegador

- A SPA é canônica em `memory.djalmajr.dev/` e nas subrotas do app; essas rotas
  entregam somente o shell sem injetar o bearer raiz.
- `/auth*` recebe login/sessão diretamente. A UI envia o cookie HttpOnly para
  `/api/v1*`, `/admin*` e `/keys*`; o engine/sidecar aplicam autorização e CSRF.
- `/mcp`, `/hook` e os demais consumidores de máquina continuam no
  `forward_auth` com bearer próprio.

### Fase 5 — remover oauth2-proxy e Keycloak

Só depois da Fase 4 provada no navegador. Nesta ordem: oauth2-proxy → Keycloak
→ keycloak-db. Decidir o `mcp-auth`: sem OAuth, sobra-lhe traduzir token; provavelmente sai.

### Fase 6 — desmontar o lab

gitlab, registry, kas, rancher, minio, minio-api, tenancit, tenancit-id — e
então o k3s. Fazer backup do que for guardar **antes**. Os três registros A
`proxied=false` somem junto, tirando o IP da origem de circulação.

## Riscos

| Risco | Mitigação |
|---|---|
| Dois engines escrevendo o mesmo SQLite | Fase 1 usa cópia; Fase 2 só sobe após `--replicas=0` |
| git-mirror empurrar do clone de teste | dry-run / sem chave na Fase 1 |
| Lockout do navegador na Fase 4 | `/admin` e `/mcp` fora do Access; SSH nunca depende de HTTP |
| Perda de dados na cópia | `ai-memory backup` na Fase 1 + o espelho no GitHub |
| Regra do túnel errada | rollback é uma linha, testável em segundos |
