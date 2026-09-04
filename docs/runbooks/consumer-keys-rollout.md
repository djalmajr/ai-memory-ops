# Runbook — área administrativa + chaves por consumidor

Rollout da SPA administrativa (`ai-memory-ui`) e do subsistema de chaves por
consumidor do `mcp-auth` (issue #9) no deploy pessoal (Hetzner).

O compose de produção **não vive neste repositório** — ele está no servidor, em
`/opt/ai-memory/compose.yml`. Este runbook descreve as mudanças a aplicar lá.

## 0. Estado (atualizado 2026-09-04)

**Atualização 2026-09-04 — engine 2.0.2 (registro completo em ai-memory
`djalmajr/infra` → `registros/2026-09-04-upgrade-2.0.2-orcarouter-penpot`):**

- Engine **2.0.2** (`engine_ref=v2.0.2`, tag empurrada para o fork), imagem
  `ghcr.io/djalmajr/ai-memory-ops/ai-memory@sha256:9258df5e217f9cc1c2847c3a2f3ba0155fa2ab2293c6237f403e0785cc5ce51a`
  (workflow `33901850223`, `ui_ref=aa3c367…`). Wiki migrado para **OKF v0.2**
  (backup automático em `/data/backups/ai-memory-backup-okf-v0.2-20260904-184038.tar.gz`).
- ⚠️ A imagem anterior vinha do commit de branch `9f1762ad` (`feat/admin-console`),
  cujas migrations V51/V52 foram **renumeradas** no merge upstream (#533 → V54/V55).
  A 2.0.2 recusou o store; o `refinery_schema_history` foi realinhado com script
  cirúrgico (`/opt/ai-memory/v2fix/fix_history.py`) após ensaio num clone dos dados.
  **Regra:** deployar só tags oficiais que já contenham o merge; nunca build de
  branch com migration própria.
- `compose.yml`: `AI_MEMORY_IN_CONTAINER: "1"`; LLM migrado para **OrcaRouter**
  (`openai-compat`, `z-ai/glm-5.3-flash`, `https://api.orcarouter.ai/v1`,
  chave em `LLM_API_KEY`). Embeddings seguem no Cloudflare `bge-m3` via
  `OPENAI_API_KEY`; embeddings locais da 2.0 **não** ativados (modelo só inglês).
- Auto-improve **ligado** e automático: `AI_MEMORY_AUTO_IMPROVE__SCHEDULER__ENABLED=true`,
  `AI_MEMORY_AUTO_IMPROVE__REQUIRE_APPROVAL=false` (no `.env`).
- 22 diretórios órfãos do wiki (projetos purgados) movidos para
  `/opt/ai-memory/orphans-20260904/`; linha órfã em `auto_improve_scheduler_state`
  removida (FK check limpo). Penpot removido do host e do túnel.
- Backups: `/opt/ai-memory/backup-pre-v2-20260904T174055Z.tar.gz` e
  `{compose.yml,.env}.bak-v2-20260904T1750Z`.

**Estado anterior (2026-08-30) — mantido como histórico:**


**Aplicado em produção:**

- SPA administrativa canônica em `https://memory.djalmajr.dev/`, servida pelo
  engine 1.37.0. `/login` usa usuário/senha e cookie `ai_memory_session`;
  `/web` e `/web/...` respondem 308 para as rotas equivalentes na raiz.
- `mcp-auth` em modo `keys-only`, com banco no volume nomeado
  `ai-memory_mcp-auth-keys`; `/keys*` é roteado diretamente ao sidecar e
  sessões são introspectadas pelo engine via Compose DNS.
- Caddy em `127.0.0.1:8080` deixa SPA e `/auth*` públicos sem Authorization
  nem actor headers, fecha `/internal/*` com 404 e mantém `forward_auth` nas
  rotas de máquina/dados. O túnel Cloudflare aponta para essa porta.
- Chave `operator` (`read,write,admin`, owner `subject`) e chaves dedicadas
  `claude-code`, `cursor`, `codex` e `omp` (`read,write`) emitidas. Cada cliente
  leu, escreveu e apagou uma página com atribuição `djalmajr`.
- Os quatro CLIs usam somente suas chaves `amk_`. Claude Code e OMP também
  receberam hooks com credencial dedicada.
- `AI_MEMORY_AUTH_TOKEN`, `ACTOR_PROXY_BEARER_TOKEN` e `HOOK_AUTH_TOKEN` foram
  rotacionados e são distintos. `PASSTHROUGH_UNKNOWN_BEARER=0`; o bearer raiz
  anterior e bearers desconhecidos respondem 401 na borda.
- Root humano `djalmajr` bootstrapped com troca obrigatória de senha.
  `AI_MEMORY_AUTH__INITIAL_ROOT_PASSWORD` já foi removida do compose/processo;
  recovery e o pepper estável de credenciais `aim_` permanecem configurados.
- O spool local legado, que havia atingido 10.000 eventos sem autenticação, foi
  autenticado, replayado e drenado antes da rotação. Sete `PreCompact` presos
  foram recuperados com o provider LLM temporariamente desativado, evitando o
  timeout de 300 s por evento; a configuração normal foi restaurada em seguida.

Backups operacionais incluem
`/opt/ai-memory/{.env,compose.yml,Caddyfile}.bak-root-gate3-20260830T200320Z`
e o snapshot verificado
`/opt/ai-memory/backup-pre-root-gate3-20260830T200320Z.tar.gz`
(SHA-256 `518618e07175dfe1d8a1c69be952d86451e1bb8ab3605b14b227469b44dc6b74`),
além dos backups históricos listados nas seções de rollout anteriores.

A imagem ativa (até 2026-09-04) era
`ghcr.io/djalmajr/ai-memory-ops/ai-memory@sha256:8b78527bef5a55b6bb33bd28a855551a9b1885ba717889ff51396fd1719c7621`,
construída no workflow `33335168979` a partir do engine
`9f1762ad2046564be0e8a686718ca2bb0014dc40` e da UI
`aa3c367eabe5dc8479aa0628fc37af89e6f1d8a5`. O rollout preservou
`/opt/ai-memory/compose.yml.bak-recovery-copy-20260830T210912Z`.

**Gate 3 aplicado:** engine/UI e sidecar estão presos por digest; a SPA usa
sessão humana na raiz, o Caddy não recebe/injeta o bearer raiz e `/web` existe
somente como redirect legado. Credencial inicial/recovery foi registrada em
arquivo root-only no host; nenhum valor entra neste repositório.

## 1. Build da SPA

```bash
cd ~/Developer/djalmajr/ai-memory-ui
npm ci
npm run build          # i18n + tsr generate + tsc -b + vite build → dist/
```

Gate local antes de publicar (todos verdes):

```bash
npx vitest run                                   # 126 testes
npx playwright test e2e/login.spec.ts e2e/shell.spec.ts   # 14, modo fixtures
```

## 2. Publicar a SPA — ela é **buildada dentro da imagem**, não copiada

`images/ai-memory/Dockerfile` clona `ai-memory-ui` do GitHub e roda o build no
estágio `ui`, com `COPY --from=ui /ui/dist /web-ui`. O compose de produção
**não monta volume em `/web-ui`** — a SPA vem da imagem. Então não existe
`rsync dist/` neste deploy: publicar a UI é

1. push na `main` do `ai-memory-ui` **e** `engine_ref`/`ui_ref` imutáveis
   registrados no preflight (SHA de 40 caracteres ou release tag);
2. `workflow_dispatch` de `build-images.yml` com `image=ai-memory` e esses
   refs. Push em `images/mcp-auth/**` **não** reconstrói `ai-memory`. Dispatch
   **nunca** empurra `latest`;
3. apontar o compose para o **digest** candidato e recriar o container no
   Gate 3, nunca `ai-memory:latest`.

```bash
# Use a tag único exibido pelo workflow dispatch; nunca resolva `latest`.
CANDIDATE_TAG='candidate-<run-id>-<run-attempt>'
IMAGE="ghcr.io/djalmajr/ai-memory-ops/ai-memory:${CANDIDATE_TAG}"
docker pull --platform linux/amd64 "$IMAGE"
docker inspect --format '{{index .RepoDigests 0}}' "$IMAGE"

# no servidor: backup, troca do digest, recriação
cp /opt/ai-memory/compose.yml /opt/ai-memory/compose.yml.bak-pre-adminui
sed -i 's|ai-memory@sha256:<antigo>|ai-memory@sha256:<novo>|' /opt/ai-memory/compose.yml
cd /opt/ai-memory && docker compose pull ai-memory && docker compose up -d ai-memory
```

O engine lê o `index.html` uma vez, no boot: recriar o container é o que troca a
SPA. (Num deploy que *monte* `/web-ui` de um diretório do host, publicar sem
reiniciar deixa o HTML antigo em memória apontando para hashes que não existem
mais e a tela fica **branca** com
`Failed to load module script: ... MIME type of "text/html"` — reproduzido duas
vezes na validação. Com a SPA na imagem, o problema não existe.)

Verificação (aplicada em 2026-08-29):

```bash
docker ps --filter name=ai-memory-ai-memory-1 --format '{{.Status}}'   # Up (healthy)
docker exec ai-memory-ai-memory-1 sh -c 'ls /web-ui/assets | grep -cE "^(access|consumers|login|users|ops)-"'
curl -s -u ":$ROOT_TOKEN" https://memory.djalmajr.dev/web/ | grep -o 'assets/index-[^"]*\.js'
```

## 3. Subir o `mcp-auth` com chaves e rotear `/keys*`

Imagem nova (multi-arch, conforme `images/mcp-auth/README.md`), então no
compose do servidor:

```yaml
  mcp-auth:
    image: ghcr.io/djalmajr/ai-memory-ops/mcp-auth@sha256:<digest>
    restart: unless-stopped
    env_file: [.env]
    environment:
      # SEM `OIDC_ISSUER`: este host não tem Keycloak (inventário em
      # `djalmajr/infra` services.md o lista como receita NÃO implantada). O
      # sidecar sobe em `mode: keys-only` — `amk_` é resolvido localmente e uma
      # chave com escopo `admin` emite as outras. Se um dia houver Keycloak,
      # basta setar o issuer aqui.
      KEYS_DB: /data/keys.db
      # MESMO valor já em AI_MEMORY_AUTH__ACTOR_PROXY_BEARER_TOKEN no engine —
      # e OBRIGATORIAMENTE distinto de AI_MEMORY_AUTH_TOKEN. O engine testa o
      # root ANTES do proxy (`auth.rs:329-331`: "Actor assertion headers are
      # intentionally ignored here; only the distinct proxy credential may
      # assert them"), então tokens iguais fazem toda identidade traduzida
      # entrar como Root, sem atribuição, em silêncio.
      ACTOR_PROXY_BEARER_TOKEN: ${ACTOR_PROXY_BEARER_TOKEN}
      # Fonte única: `.env`. Sondado no servidor — a linha do engine é
      # `AI_MEMORY_AUTH__ACTOR_PROXY_BEARER_TOKEN: ${ACTOR_PROXY_BEARER_TOKEN}`,
      # interpolação, não literal (o valor não aparece em `compose.yml`; só em
      # `.env`). A fonte é única, mas a rotação exige recriar engine,
      # mcp-auth E Caddy para os três receberem o valor novo. Confirme com o
      # check da seção "Conferência de tokens" abaixo.
      # Sem isto os branches de hook e OIDC ecoam o token do chamador, que não
      # entra no rung de proxy: medido, `POST /hook` devolve 401 (ausente) vs
      # 202 com `actor_user: user:djalmajr` (setado com o token de proxy).
      UPSTREAM_AUTH_TOKEN: ${ACTOR_PROXY_BEARER_TOKEN}
      # Migração encerrada: bearers desconhecidos falham fechado.
      PASSTHROUGH_UNKNOWN_BEARER: "0"
      # OAuth/DCR desligado: o handler de metadata RFC 9728 responde 404 até
      # OAUTH_ENABLED=true. A rota do Caddy existe de qualquer forma, para o
      # 404 vir do sidecar (quem serve o endpoint) e não do engine.
      # OAUTH_ENABLED: "true"
      # OAUTH_RESOURCE: https://memory.djalmajr.dev
      # Gate 2+: introspect sessions for /keys*. URL is Compose DNS, never
      # Caddy :8080. HOST must already be in AI_MEMORY_ALLOWED_HOSTS.
      ENGINE_INTERNAL_URL: http://ai-memory:49374
      ENGINE_INTERNAL_HOST: ${ENGINE_INTERNAL_HOST}
    volumes:
      - mcp-auth-keys:/data
```

### Conferência de tokens

Compara os valores **resolvidos por serviço**, em memória, e imprime só o
veredito — sem valor e sem hash. Fingerprint de segredo vivo não entra em
arquivo versionado: um prefixo de hash é oráculo de confirmação para um token
vazado, mesmo sem permitir derivá-lo.

```bash
cd /opt/ai-memory
docker compose config --format json | python3 -c '
import json,sys
s=json.load(sys.stdin).get("services",{})
def env(svc,key): return (s.get(svc,{}).get("environment") or {}).get(key)
pe=env("ai-memory","AI_MEMORY_AUTH__ACTOR_PROXY_BEARER_TOKEN")
rt=env("ai-memory","AI_MEMORY_AUTH_TOKEN")
ps=env("mcp-auth","ACTOR_PROXY_BEARER_TOKEN")
us=env("mcp-auth","UPSTREAM_AUTH_TOKEN")
cw=env("caddy","AI_MEMORY_WEB_UPSTREAM_TOKEN")
print("proxy token no engine :", "presente" if pe else "AUSENTE")
print("proxy != root         :", "DISTINCT" if pe and pe != rt else "SAME/AUSENTE")
print("engine == sidecar     :", ("MATCH" if pe==ps==us else "DIFFER") if (ps or us) else "n/a")
print("caddy web token       :", "AUSENTE" if not cw else "PRESENTE")
print("engine internal url   :", "presente" if env("mcp-auth","ENGINE_INTERNAL_URL") else "AUSENTE")
print("engine internal host  :", "presente" if env("mcp-auth","ENGINE_INTERNAL_HOST") else "AUSENTE")
print("trusted proxy cidrs   :", "presente" if env("ai-memory","AI_MEMORY_AUTH__TRUSTED_PROXY_CIDRS") else "AUSENTE")
print("token pepper         :", "presente" if env("ai-memory","AI_MEMORY_AUTH__TOKEN_PEPPER") else "AUSENTE")
print("initial root password:", "AUSENTE" if not env("ai-memory","AI_MEMORY_AUTH__INITIAL_ROOT_PASSWORD") else "PRESENTE")
print("recovery token        :", "presente" if env("ai-memory","AI_MEMORY_AUTH__RECOVERY_TOKEN") else "AUSENTE")
'
```

Lê o compose **resolvido**, então cobre literal e `${...}` igualmente, e não
depende de o `.env` estar exportado no shell.

**Hoje (Gate 3 aplicado):** `engine == sidecar: MATCH`,
`proxy != root: DISTINCT`, `caddy web token: AUSENTE`, internal URL/host,
trusted proxy CIDR, recovery e token pepper presentes. A senha inicial fica
ausente depois do bootstrap. `PRESENTE` no Caddy significa regressão: o inject
de raiz voltou e a SPA password-only deixou de ser pública de verdade.

`DIFFER` em `engine == sidecar` faz toda chave `amk_` retornar 401; `SAME` em
`proxy != root` transforma toda identidade traduzida em Root sem atribuição.

Nota de higiene: o commit `223927b` deste repo (público) registrou por engano um
prefixo de 12 hex do SHA-256 do token de proxy vivo. Já saiu do HEAD. Um
prefixo de 48 bits **não permite derivar** um bearer aleatório — só confirmaria
um candidato que o atacante já tivesse — e `grep` no servidor não prova ausência
de cliente externo com o token, então a rotação **não** é emergencial: ela entra
na janela do passo 5, junto da rotação do token raiz e do hook. Fingerprint de
segredo vivo não volta a arquivo versionado.

> **A topologia atual não tem onde pendurar o forwardAuth.** Verificado no
> servidor: nada escuta em 80/443; o único listener é
> `127.0.0.1:49374` (o engine), e o `cloudflared` roda no host apontando o
> hostname direto para ele. Não há Traefik, Caddy, nginx nem oauth2-proxy.
>
> Regra de ingress de túnel resolve **roteamento por caminho**, e nada mais. O
> contrato do `/verify` exige uma subrequest de autenticação que **substitui** o
> `Authorization` e injeta `X-Memory-Actor-*` — isso é `forward_auth` (Caddy),
> `auth_request` (nginx) ou `forwardAuth` (Traefik), não ingress de túnel.
>
> **Nota (2026-08-30):** a topologia acima é **histórica**. Caddy já escuta em
> `127.0.0.1:8080` e o túnel aponta para essa porta (seção 0). O Gate 3 troca
> o Caddyfile aplicado pelo de destino; rollback do ingresso continua
> reapontar para `http://127.0.0.1:49374`.

> **Como reapontar o túnel** (de `djalmajr/infra` →
> `runbooks/04-cloudflare-tunnel.md`): o ingress é gerenciado por API, tunnel
> `dev` = `c0a4c887-7a8d-42d9-bfa2-f7318298ff37`, e hoje
> `memory.djalmajr.dev` → `http://localhost:49374`. Alvo novo:
> `http://localhost:8080` (o Caddy).
>
> ```bash
> HOST=memory ORIGIN=http://127.0.0.1:8080 ./scripts/add-tunnel-host.sh
> ```
>
> Precisa de `CLOUDFLARE_API_TOKEN` + `CLOUDFLARE_ACCOUNT_ID`. **Nunca** rode
> `scripts/setup-cloudflare-tunnel.sh` com `HOSTS` parcial: ele faz PUT da lista
> inteira e apagaria `rancher`/`penpot`. Rollback = rodar o mesmo comando com
> `ORIGIN=http://127.0.0.1:49374`.

### Proxy Caddy — config **aplicada** (2026-08-29), inject de raiz em `/web*`

O container recebe somente a credencial interna necessária para servir a SPA;
não use `env_file`, que exporia ao Caddy todas as credenciais de providers.
Este bloco é o estado **vivo** até o Gate 3. O alvo (sem inject) vem a seguir.

```yaml
  caddy:
    environment:
      AI_MEMORY_WEB_UPSTREAM_TOKEN: ${AI_MEMORY_AUTH_TOKEN}
```


```caddyfile
:8080 {
	# /keys* vai direto ao sidecar: ele É o serviço de auth; passá-lo pelo
	# próprio forward_auth devolve 401.
	handle /keys* {
		reverse_proxy mcp-auth:8081
	}

	# O documento e os assets da SPA são públicos. O engine exige auth até para
	# servir HTML, então o Caddy autentica SOMENTE este upstream com o root;
	# o token nunca volta ao browser e Bearer não emite cookie. As APIs continuam
	# no forward_auth abaixo e a SPA mostra /login quando não há chave local.
	handle /web* {
		reverse_proxy ai-memory:49374 {
			header_up Authorization "Bearer {$AI_MEMORY_WEB_UPSTREAM_TOKEN}"
		}
	}

	# Metadata RFC 9728: quem serve é o SIDECAR (`main.go:134`); o engine não
	# tem esse path (zero ocorrências). Sem esta rota o request cai no
	# catch-all, o /verify libera como público e o Caddy manda ao engine → 404,
	# matando a descoberta de IdP que a SPA faz em `api.ts:39`.
	handle /.well-known/oauth-protected-resource {
		reverse_proxy mcp-auth:8081
	}


	# Catch-all como `handle` (fica no grupo de exclusão mútua dos handles
	# acima), com `route` DENTRO dele — `route` é outra diretiva, e um `route`
	# no topo dependeria de o `reverse_proxy` dos handles ser terminal para não
	# ser avaliado também. O nesting não depende dessa interação.
	#
	# `route` preserva a ordem do arquivo (fora dele o Caddy reordena
	# diretivas). Strip explícito ANTES do forward_auth, defesa em
	# profundidade: se algum dia alguém encurtar o `copy_headers`, os cinco
	# continuam sendo descartados.
	handle {
		route {
			request_header -X-Memory-Actor-User
			request_header -X-Memory-Actor-Sub
			request_header -X-Memory-Actor-Issuer
			request_header -X-Memory-Actor-Client
			request_header -X-Memory-Actor-Agent

			forward_auth mcp-auth:8081 {
				uri /verify
				copy_headers Authorization X-Memory-Actor-User X-Memory-Actor-Sub X-Memory-Actor-Issuer X-Memory-Actor-Client X-Memory-Actor-Agent
			}
			reverse_proxy ai-memory:49374
		}
	}
}
```

### Caddy/compose de destino (Stories 05–07) — **não** recarregar o tráfego até o Gate 3

Validar sintaxe no servidor (`caddy validate --config`) sem apontar o túnel.
Não existe janela pública em que a SPA password-only fique atrás deste Caddy
antigo (`/auth/login` receberia 401). Engine/UI novo + este Caddy entram no
mesmo gate de manutenção.

A lista de `copy_headers` continua o ponto de segurança: é **autoritativa**.
Header presente na resposta do `/verify` é setado; header **ausente é
removido**. Enumere os cinco actor headers. **Nunca** use
`request_header -X-Memory-Actor-*`: o wildcard apaga
`X-Memory-Actor-Session-Id` (auto-scope de hook/workstream). Authorization é
decidido no sidecar, que examina todos os valores do header, reconhece Bearer
sem diferenciar maiúsculas/minúsculas e recusa tentativas vazias, HTAB ou
duplicadas sem cair para cookie. Não filtre o esquema no Caddy: isso cria uma
segunda implementação do parser HTTP.

Compose alvo (aplicar no Gate 3; o sidecar já pode receber `ENGINE_INTERNAL_*`
no Gate 2):

```yaml
  mcp-auth:
    environment:
      ENGINE_INTERNAL_URL: http://ai-memory:49374
      ENGINE_INTERNAL_HOST: ${ENGINE_INTERNAL_HOST}

  ai-memory:
    environment:
      # A SPA é canônica na raiz do host; APIs mantêm os próprios paths.
      AI_MEMORY_WEB_SLUG: /
      # CIDR do peer Caddy na rede Docker, medido no host. Não use um range
      # privado amplo; XFF de peer não confiável não pode escolher o bucket
      # de login.
      AI_MEMORY_AUTH__TRUSTED_PROXY_CIDRS: ${AI_MEMORY_AUTH__TRUSTED_PROXY_CIDRS}
      AI_MEMORY_AUTH__INITIAL_ROOT_PASSWORD: ${AI_MEMORY_AUTH__INITIAL_ROOT_PASSWORD}
      AI_MEMORY_AUTH__RECOVERY_TOKEN: ${AI_MEMORY_AUTH__RECOVERY_TOKEN}
      # Pepper estável para hashes das credenciais nativas aim_. Deve existir
      # antes da primeira key e nunca ser rotacionado sem revogar todas.
      AI_MEMORY_AUTH__TOKEN_PEPPER: ${AI_MEMORY_AUTH__TOKEN_PEPPER}

  caddy:
    environment:
      # Removido: AI_MEMORY_WEB_UPSTREAM_TOKEN. As rotas públicas da SPA não
      # injetam o bearer raiz.
```

Caddy de destino — handles mutuamente exclusivos; subrotas públicas da SPA,
`/auth`, `/keys` e `/internal` ficam **fora** do `route` do catch-all. A lista
`@spa_routes` acompanha as rotas declaradas pelo `ai-memory-ui`; rotas de
máquina e dados continuam no `forward_auth`:

```caddyfile
:8080 {
	handle /internal/* {
		respond 404
	}

	handle /web {
		redir * / 308
	}

	@legacy_web path_regexp legacy_web ^/web/+(.*)$
	handle @legacy_web {
		redir * /{re.legacy_web.1} 308
	}

	@spa_routes {
		path / /index.html /favicon.ico /favicon.svg /icons.svg /assets/* /login /login/* /workspaces /workspaces/* /projects /projects/* /s /s/* /access /access/* /backups /backups/* /config /config/* /ops /ops/* /consumers /consumers/* /users /users/* /activity /activity/* /audit /audit/* /sessions /sessions/* /graph /graph/*
	}
	handle @spa_routes {
		request_header -Authorization
		request_header -X-Memory-Actor-User
		request_header -X-Memory-Actor-Sub
		request_header -X-Memory-Actor-Issuer
		request_header -X-Memory-Actor-Client
		request_header -X-Memory-Actor-Agent
		reverse_proxy ai-memory:49374 {
			header_up -Authorization
			header_up -X-Memory-Actor-User
			header_up -X-Memory-Actor-Sub
			header_up -X-Memory-Actor-Issuer
			header_up -X-Memory-Actor-Client
			header_up -X-Memory-Actor-Agent
		}
	}

	handle /auth* {
		request_header -Authorization
		request_header -X-Memory-Actor-User
		request_header -X-Memory-Actor-Sub
		request_header -X-Memory-Actor-Issuer
		request_header -X-Memory-Actor-Client
		request_header -X-Memory-Actor-Agent
		reverse_proxy ai-memory:49374 {
			header_up -Authorization
			header_up -X-Memory-Actor-User
			header_up -X-Memory-Actor-Sub
			header_up -X-Memory-Actor-Issuer
			header_up -X-Memory-Actor-Client
			header_up -X-Memory-Actor-Agent
		}
	}

	handle /keys* {
		reverse_proxy mcp-auth:8081
	}

	handle /.well-known/oauth-protected-resource {
		reverse_proxy mcp-auth:8081
	}

	handle {
		route {
			request_header -X-Memory-Actor-User
			request_header -X-Memory-Actor-Sub
			request_header -X-Memory-Actor-Issuer
			request_header -X-Memory-Actor-Client
			request_header -X-Memory-Actor-Agent

			forward_auth mcp-auth:8081 {
				uri /verify
				copy_headers Authorization X-Memory-Actor-User X-Memory-Actor-Sub X-Memory-Actor-Issuer X-Memory-Actor-Client X-Memory-Actor-Agent
			}
			reverse_proxy ai-memory:49374
		}
	}
}
```

`X-Memory-Actor-Session-Id` fica de propósito fora de `copy_headers` e do
strip: o sidecar nunca o emite; hook/`mcp_bridge` o enviam; listá-lo faria o
Caddy apagar a sessão legítima. Forjá-lo não escala privilégio (cache key).

Antes do Gate 3, repetir no ensaio isolado:

1. `/internal/*` → 404 na borda; chamada Compose-DNS + Host allowlisted +
   proxy bearer sem actor headers alcança o handler do engine.
2. `/`, as subrotas de `@spa_routes` e `/auth*` chegam ao engine **sem**
   Authorization e sem os cinco actor headers; `GET /login` = 200 sem
   `WWW-Authenticate`. `/web` = 308 para `/`, `/web/login` = 308 para
   `/login` e barras repetidas nunca produzem `Location` iniciado por `//`.
3. `/keys*` entrega todos os valores de `Authorization` ao sidecar; ele descarta
   Basic/outro, mas qualquer tentativa Bearer — inclusive minúscula, vazia,
   HTAB ou duplicada — vence o cookie e falha fechada se inválida.
4. Cookie `ai_memory_session` atravessa `/verify` sem identidade; cookie
   forjado também 200 no sidecar e 401 no engine.
5. Bearer inválido + cookie **não** cai para sessão.
6. `/hook` com `X-Memory-Actor-Session-Id` preserva auto-scope.
7. Cinco actor headers forjados chegam só com o valor verificado pelo sidecar.

## 4. Migrar consumidores (já aplicado — CLIs antes do hook)

Os quatro CLIs (`claude-code`, `cursor`, `codex`, `omp`) já usam chaves `amk_`
dedicadas. Não reemitir no cutover. Gate 4 revoga **somente** keys coladas no
browser antigo (`operator` se aplicável). O token de hook continua o último a
sair: quando `/hook` 401a, o spool enche até o cap e o drain oldest-first
bloqueia captura nova.

Antes de qualquer rotação de hook: `POST /hook` com o token atual deve
devolver **202** com `actor_user`. Se 401ar, esvazie o spool antes de seguir.

## 5. Verificação de borda

```bash
B=https://memory.djalmajr.dev

# chave amk_ dedicada: 200 e escrita atribuída (antes e depois do Gate 3)
curl -s -o /dev/null -w '%{http_code}\n' -H "Authorization: Bearer amk_..." $B/api/v1/workspaces

# esquema é case-insensitive: a mesma chave continua 200
curl -s -o /dev/null -w '%{http_code}\n' -H "Authorization: bearer amk_..." $B/api/v1/workspaces

# qualquer tentativa Bearer inválida vence o cookie e fica 401
curl -s -o /dev/null -w '%{http_code}\n' -b "$COOKIE_JAR" \
  -H 'Authorization: bearer invalid' $B/api/v1/workspaces
curl -s -o /dev/null -w '%{http_code}\n' -b "$COOKIE_JAR" \
  -H 'Authorization: Bearer' $B/api/v1/workspaces
curl -s -o /dev/null -w '%{http_code}\n' -b "$COOKIE_JAR" \
  -H 'Authorization: Basic Zm9vOmJhcg==' -H 'Authorization: Bearer invalid' \
  $B/api/v1/workspaces

# chave revogada: 401
curl -s -o /dev/null -w '%{http_code}\n' -H "Authorization: Bearer amk_<revogada>" $B/api/v1/workspaces

# ator forjado pelo cliente: o engine recusa (400) ou ignora — nunca aceita
curl -s -o /dev/null -w '%{http_code}\n' \
  -H "Authorization: Bearer amk_..." -H 'X-Memory-Actor-User: fulano' $B/api/v1/workspaces
```

Depois do Gate 3, no ensaio interno (não imprimir cookie/CSRF):

```bash
# raiz e login públicos, sem Authorization
curl -sI http://127.0.0.1:8080/ | grep -Ei 'HTTP/|www-authenticate'
curl -sI http://127.0.0.1:8080/login | grep -Ei 'HTTP/|www-authenticate'

# aliases legados redirecionam para as rotas canônicas
curl -sI http://127.0.0.1:8080/web | grep -Ei 'HTTP/|location'
curl -sI http://127.0.0.1:8080/web/login | grep -Ei 'HTTP/|location'
curl --path-as-is -sI http://127.0.0.1:8080/web//evil.example | grep -Ei 'HTTP/|location'

# /internal 404 na borda
curl -s -o /dev/null -w '%{http_code}\n' http://127.0.0.1:8080/internal/auth/session-introspect

# Basic salvo não bloqueia o catch-all quando há cookie (sidecar 200, engine decide)
# Bearer vazio/malformado ignora cookie (sidecar 401)
```

## 6. Smoke da área administrativa — hoje vs Gate 3

**Hoje (key login, Caddy com inject de raiz):** a suíte `e2e/live.spec.ts` é
opt-in. A chave administrativa entra só no `localStorage` do browser de teste:

```bash
cd ~/Developer/djalmajr/ai-memory-ui
E2E_BASE_URL=https://memory.djalmajr.dev/web \
E2E_ADMIN_TOKEN=<chave amk_ operator> \
E2E_USER_TOKEN=<chave amk_ read,write> \
E2E_SCOPE_PATH=/s/djalmajr/ai-memory \
npx playwright test e2e/live.spec.ts
```

**Gate 3 (sessão + CSRF, UiHumanAuth/EngineHumanAuth):** usar
`E2E_BASE_URL=https://memory.djalmajr.dev`. Login por username/password,
`credentials: include`, CSRF em POST/DELETE de `/keys`. `E2E_BASE_URL` é
obrigatório no live guard — falha, não skip. Não colar `amk_` no browser como
fallback. Rotação de consumidores continua create+revoke; não existe
`/keys/{id}/rotate`.

## 7. Contrato de autenticação da UI

**Hoje:** `/web*` é público **somente para carregar a SPA**. O Caddy autentica
essa rota internamente com `AI_MEMORY_WEB_UPSTREAM_TOKEN` =
`${AI_MEMORY_AUTH_TOKEN}`. Sem chave no `localStorage`, `/api/v1` 401 e o
guard leva a `/login`.

**Gate 3:** o inject some. As rotas de `@spa_routes` e `/auth*` chegam ao
engine sem Authorization; `/web` apenas redireciona para a rota canônica.
Cookie HttpOnly `ai_memory_session` + CSRF. `/keys*` no sidecar introspecta a
sessão via `POST ${ENGINE_INTERNAL_URL}/internal/auth/session-introspect`
(`Host=ENGINE_INTERNAL_HOST`, `Authorization: Bearer ${ACTOR_PROXY_BEARER_TOKEN}`,
sem cookie e sem `X-Memory-Actor-*`). Owner de chave emitida por sessão:
`{kind:user,label:username}`. `amk_` admin segue sem chamar o engine. `aim_`
só no catch-all `/verify` (eco); em `/keys*` é 403.

## 8. Gates, digests e rollback (Story 07)

Este repo **não** muta `/opt`. O corte público é operacional no servidor.

### Digest e workflow

- Push em `images/<name>/` publica **somente** essa imagem. `ai-memory`
  **nunca** sai de push/tag.
- `workflow_dispatch` `image=ai-memory` exige `engine_ref` + `ui_ref`
  imutáveis (SHA de 40 chars ou tag `vX.Y.Z`). `main`/`latest`/vazio
  rejeitados. Dispatch **nunca** empurra `latest`.
- `images/ai-memory/Dockerfile` busca o ref diretamente e aceita os dois
  formatos validados pelo workflow: SHA de 40 caracteres ou release tag.
- Gate 2 retargeta só o digest de `mcp-auth`. Não puxar `ai-memory:latest`.
- Gate 3 aponta compose para digest candidato registrado no preflight.

### Gate 0 — preflight

Registrar digests atuais (engine/UI, sidecar, Caddy), commits candidatos
`AI_MEMORY_REF`/`AI_MEMORY_UI_REF`, backup verificado do SQLite do engine e
do `KEYS_DB`, cópia de Caddyfile/compose/inventário de **nomes** de variáveis
(nunca valores). Confirmar por presença (não por hash impresso) que
`AI_MEMORY_AUTH_TOKEN`, `ACTOR_PROXY_BEARER_TOKEN`, recovery, senha inicial e
token pepper são distintos. Preparar
`AI_MEMORY_AUTH__INITIAL_ROOT_PASSWORD`, `AI_MEMORY_AUTH__RECOVERY_TOKEN`,
`AI_MEMORY_AUTH__TOKEN_PEPPER` (64 hex, estável),
`AI_MEMORY_AUTH__TRUSTED_PROXY_CIDRS` (peer Caddy),
`ENGINE_INTERNAL_URL=http://ai-memory:49374` e `ENGINE_INTERNAL_HOST` já em
`AI_MEMORY_ALLOWED_HOSTS`. Comprovar os quatro clientes `amk_`.

### Gate 1 — ensaio isolado

Restaurar backup em volume/rede/porta não públicos. Exercitar migrations
V51/V52, bootstrap, troca, recovery, Users, Access, borda de sessão e
Consumers. Provar SPA pública e browser wiki autenticado. Destruir o volume
de ensaio; registrar só resultados.

### Gate 2 — sidecar backward-compatible

Publicar `mcp-auth` (path-selective). Implantar o digest novo com cookie
passthrough, descarte de Basic, branch `aim_`, `ENGINE_INTERNAL_*`. Bearers
atuais continuam na primeira branch. Introspecção **live falha fechada** até
o engine do Gate 3 expor `/internal/auth/session-introspect`. Validar Caddy
de destino sem recarregar tráfego. Pré-puxar só digests candidatos.

### Gate 3 — corte coordenado

1. Manutenção no ingresso.
2. Parar o engine antigo; snapshot pré-migration.
3. Compose/env com senha inicial, recovery, token pepper, CIDR e
   `ENGINE_INTERNAL_*` presentes; subir engine/UI pelo digest candidato.
4. Trocar Caddy pelo de destino; remover `AI_MEMORY_WEB_UPSTREAM_TOKEN`.
5. Tirar manutenção; smoke pelo domínio.
6. Falha → manutenção, restaurar Caddy/compose/digests/snapshot. Nunca
   reabrir `/admin` anonimamente.

Não há dual-mode de key login nem janela aceita de `/auth/login=401`.

### Gate 4 — credenciais

Remover `AI_MEMORY_AUTH__INITIAL_ROOT_PASSWORD` após a troca. Recovery
permanece break-glass. Rotacionar só keys coladas no browser antigo.
Rotacionar `AI_MEMORY_AUTH_TOKEN` só se houve uso humano, distinto do
actor-proxy. SPA apaga `ai-memory-ui.token`; engine expira `ai_memory_auth`.

### Gate 5 — soak e V53

Soak objetivo (login, Users, Access, Consumers, quatro `amk_`, Caddy sem
inject/wildcard, captures sem Authorization em web/auth). Só então V53
(drop `users.token_hash`). Snapshot pré-V53 é a única base de rollback para
binário antigo. Sem versão/tag/release sem aprovação explícita.

### Rollback por fronteira

| Fronteira | Procedimento | Perda esperada |
|---|---|---|
| Antes do Gate 3 | Voltar sidecar; engine/Caddy antigos permanecem | Nenhuma mudança de usuário |
| Depois de V51/V52 e antes de V53 | Manutenção; voltar Caddy/compose/digests | Senha/sessão não existem no UI antigo |
| Falha só em Consumers | Voltar sidecar/rota `/keys`; sessão humana permanece | Gestão por sessão indisponível; `amk_` admin segue |
| Depois de V53 | Parar stack, restaurar snapshot pré-V53, depois digests/Caddy | Mudanças posteriores ao snapshot precisam reaplicar |

Rollback nunca transforma senha/recovery em Bearer e nunca publica `/admin`.

### Matriz do sidecar (Gate 2+)

| Chamada | `/verify` | `/keys*` |
|---|---|---|
| Bearer `amk_` admin | 200 + proxy bearer + actor | owner da chave; `can_issue` se scope admin |
| Bearer `aim_` | 200 eco, sem actor headers | 403 |
| Bearer JWT/`amk_` inválido | 401; cookie ignorado | 401/identidade nula; cookie ignorado |
| Bearer minúsculo ou separado por HTAB | mesma branch Bearer; nunca cookie | mesma branch Bearer; nunca cookie |
| Bearer vazio, duplicado ou ambíguo | 401; cookie ignorado | identidade nula; cookie ignorado |
| Basic / esquema desconhecido / header vazio + cookie | 200 sem Authorization/actor | introspecta sessão |
| Cookie `ai_memory_session` só | 200 sem identidade | introspecta; CSRF em POST/DELETE |
| Engine interno ausente/timeout/redirect | n/a (cookie não consulta) | 503 fail-closed |
| Cookie forjado | 200 | identidade nula / 403 conforme endpoint |

## Assunções de deploy ainda abertas

- `/opt/ai-memory/{Caddyfile,compose.yml}` não muda neste commit.
- `ENGINE_INTERNAL_HOST` já está em `AI_MEMORY_ALLOWED_HOSTS` (ex.:
  `memory.djalmajr.dev`); não adicionar DNS Compose nem `*`.
- `AI_MEMORY_AUTH__TRUSTED_PROXY_CIDRS` mede-se no host (peer Caddy).
- A rota de introspect é do EngineHumanAuth; o engine vivo provavelmente
  ainda não a expõe — Gate 2 falha fechado até o digest do Gate 3.
- Dockerfile `ai-memory` clona por `--branch`; Gate 3 usa release tags.
- Nota histórica de túnel (2026-08-30): o vivo já é Caddy em
  `127.0.0.1:8080`.
