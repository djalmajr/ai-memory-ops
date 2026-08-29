# Runbook — área administrativa + chaves por consumidor

Rollout da SPA administrativa (`ai-memory-ui`) e do subsistema de chaves por
consumidor do `mcp-auth` (issue #9) no deploy pessoal (Hetzner).

O compose de produção **não vive neste repositório** — ele está no servidor, em
`/opt/ai-memory/compose.yml`. Este runbook descreve as mudanças a aplicar lá.

## 0. Estado (atualizado 2026-08-29)

**Já aplicado em produção:**

- SPA administrativa no ar em `https://memory.djalmajr.dev/web/` — imagem
  `ai-memory@sha256:ff0a07ba…` (a SPA é buildada dentro da imagem, ver passo 2).
  Container `Up (healthy)`, zero linhas de erro no log, `/admin/status`
  respondendo. Backup do compose anterior em
  `/opt/ai-memory/compose.yml.bak-pre-adminui` (rollback = restaurar o digest
  antigo e `docker compose up -d ai-memory`).
- Imagem `mcp-auth` com o subsistema de chaves publicada em
  `ghcr.io/djalmajr/ai-memory-ops/mcp-auth` (amd64, como o resto das imagens
  deste repo) e verificada: em volume nomeado NOVO ela cria
  `keys.db{,-shm,-wal}` como `65532` e sobe sem `keys_db_open_failed`.

**Pendente, e por quê:**

- O sidecar **não está no compose** e `/keys*` responde 404 na borda: falta a
  decisão de infraestrutura do passo 3 (não existe proxy local para pendurar o
  forwardAuth — hoje o `cloudflared` aponta direto para o engine).
- Migração dos CLIs e rotação de tokens (passo 5) **não iniciada**: troca
  credencial de agente em uso e deve ser feita uma por vez, com validação.
- A SPA já degrada sozinha nesse meio-tempo: **Consumidores** mostra o banner
  "backend indisponível" com inventário vazio, nunca linhas fabricadas.
- `ACTOR_PROXY_BEARER_TOKEN` já está ativo no engine (sondado: `400
  MissingIdentity/Ambiguous` sem ator, `200` com ator válido).

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

1. push na `main` do `ai-memory-ui`;
2. rebuild da imagem `ai-memory` (o workflow `build-images.yml` cobre isso: ele
   dispara em `images/**` e a matriz inclui `ai-memory`, então um push no ops
   também reconstrói a imagem já com a SPA nova);
3. apontar o compose para o digest novo e recriar o container.

```bash
# digest publicado
docker pull --platform linux/amd64 ghcr.io/djalmajr/ai-memory-ops/ai-memory:latest
docker inspect --format '{{index .RepoDigests 0}}' ghcr.io/djalmajr/ai-memory-ops/ai-memory:latest

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
      OIDC_ISSUER: https://<keycloak>/realms/memory
      KEYS_DB: /data/keys.db
      # MESMO valor já em AI_MEMORY_AUTH__ACTOR_PROXY_BEARER_TOKEN no engine.
      ACTOR_PROXY_BEARER_TOKEN: ${ACTOR_PROXY_BEARER_TOKEN}
      # Mantém os CLIs atuais funcionando durante a migração (passo 5).
      PASSTHROUGH_UNKNOWN_BEARER: "1"
    volumes:
      - mcp-auth-keys:/data
```

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
> Portanto o passo 3 exige uma decisão de infraestrutura ainda não tomada:
> introduzir um proxy local na frente de `engine` + `mcp-auth` e reapontar o
> `cloudflared` para ele. Isso muda o único caminho de entrada do sistema em
> produção — deve ser feito com janela e rollback combinados, não de passagem.

### Proxy sugerido: Caddy — config **testada**, não proposta

```caddyfile
:8080 {
	# /keys* vai direto ao sidecar: ele É o serviço de auth; passá-lo pelo
	# próprio forward_auth devolve 401.
	handle /keys* {
		reverse_proxy mcp-auth:8081
	}

	# /web (documento + assets) sem forward_auth: o browser manda Basic, que o
	# sidecar não entende. O engine autentica isso sozinho.
	handle /web* {
		reverse_proxy ai-memory:49374
	}

	# Metadata RFC 9728: quem serve é o SIDECAR (`main.go:134`); o engine não
	# tem esse path (zero ocorrências). Sem esta rota o request cai no
	# catch-all, o /verify libera como público e o Caddy manda ao engine → 404,
	# matando a descoberta de IdP que a SPA faz em `api.ts:39`.
	handle /.well-known/oauth-protected-resource {
		reverse_proxy mcp-auth:8081
	}

	# Sessão só-cookie: a SPA no degrau `cookie-admin` não guarda chave, então
	# não manda `Authorization` — e o sidecar 401 qualquer request sem Bearer,
	# antes do engine. Quem sabe validar esse cookie é o engine, que o emitiu.
	# Restrito a GET/HEAD: o engine já recusa mutação por cookie, e aqui a
	# recusa acontece uma camada antes.
	@cookie_only {
		not header Authorization *
		header Cookie *ai_memory_auth=*
		method GET HEAD
	}
	handle @cookie_only {
		route {
			request_header -X-Memory-Actor-User
			request_header -X-Memory-Actor-Sub
			request_header -X-Memory-Actor-Issuer
			request_header -X-Memory-Actor-Client
			request_header -X-Memory-Actor-Agent
			reverse_proxy ai-memory:49374
		}
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

A lista de `copy_headers` é o ponto de segurança, e ela tem exatamente os
**cinco** headers que o `mcp-auth` emite (`main.go:481-503`). Verificado com
Caddy 2 contra um upstream de eco:

- `copy_headers` é **autoritativo sobre a lista**: header presente na resposta
  do `/verify` é setado no request; header **ausente é removido**. Forjar os
  cinco de fora resulta em só o valor verificado no upstream.
- Header de ator **fora da lista passa forjado**. Com a lista curta
  (User/Sub/Issuer), um `X-Memory-Actor-Client: forjado-cli` e um
  `X-Memory-Actor-Agent: forjado-agent` do cliente **chegaram** ao upstream —
  o engine confia nos dois (`auth.rs:1004`), então atribuição de cliente/agente
  ficaria forjável.
- Dentro de `route`, o strip explícito dos cinco roda antes e **não** derruba o
  ator verificado (testado). Fora de `route` ele roda **depois** do
  `copy_headers` e o ator chega **vazio** — se tirar o `route`, tire o strip
  junto.
- **Nunca** use o strip curinga `request_header -X-Memory-Actor-*`: ele apaga
  também o `X-Memory-Actor-Session-Id` legítimo (testado — a sessão do hook
  desaparece). Enumere os cinco.

`X-Memory-Actor-Session-Id` fica **deliberadamente fora da lista**, e isso não
é descuido:

- o sidecar nunca o emite — é reservado à sessão real de lifecycle-hook, não à
  sessão de login do provedor (`main.go:477-479`);
- quem o manda são o hook gerado (`install_hooks.rs:3475`) e o
  `mcp_bridge` (`mcp_bridge.rs:19`). Listá-lo faria o Caddy **apagar** essa
  sessão legítima em todo request, quebrando auto-scope em silêncio;
- forjá-lo não escala privilégio: o engine trata session_id de header como
  **cache key, não credencial** — o componente `user` vem só do `ActorContext`
  do middleware, nunca do header. Uma sessão forjada dá o slot
  `(user-autenticado, sessão-forjada)`, nunca o slot de outro
  (`autoscope_stress.rs:795-804`, teste
  `header_session_id_is_cache_key_not_credential`).

Isolamento de caminho, medido (o `/verify` falso logando cada subrequest):
`/keys`, `/keys/abc123`, `/web/` e `/web/assets/x.js` **nunca** chamaram o
`/verify`; só `/mcp` e `/api/v1/workspaces` chamaram. Vale nas duas formas
(`route` no topo e `handle { route { … } }`) — a forma com nesting é a do
arquivo porque não depende de handler terminal.

Consequência: `/keys*` e `/web*` **repassam** header de ator forjado pelo
cliente, porque não passam por strip nem por `copy_headers`. Medido, e **inerte
nos dois destinos** — por isso não há strip ali:

- o `mcp-auth` nunca lê `X-Memory-Actor-*` de entrada (zero ocorrências de
  `Header.Get` para eles em `main.go`/`keys.go`); o dono da chave vem sempre da
  credencial de quem chama;
- o engine só honra header de ator no degrau de **proxy confiável**. No degrau
  raiz/cookie — que é o do `/web` — eles são ignorados: teste
  `actor_headers_are_ignored_on_the_root_rung` (`auth.rs:1272-1293`) manda
  `X-Memory-Actor-User: impostor` com o token raiz e o ator continua `boss`.
  Há o par `actor_headers_are_ignored_without_a_configured_proxy_bearer`
  (`auth.rs:1243`).

Corte com risco baixo: subir Caddy + mcp-auth numa porta paralela
(`127.0.0.1:8080`) com o engine ainda publicando 49374, validar por curl e pela
suíte `live.spec.ts` apontada para `http://127.0.0.1:8080/web`, e só então
reapontar o `cloudflared`. Rollback = reapontar o túnel de volta.

Esta config foi exercitada com a cadeia inteira local — Caddy 2 + `mcp-auth`
compilado do fonte + engine 1.32.2 real servindo a SPA — e o Caddyfile do teste
saiu **deste bloco**, só trocando os alvos. Resultado:

| Request | Quem respondeu |
|---|---|
| `/.well-known/oauth-protected-resource` | sidecar, metadata real |
| `/keys` sem credencial | sidecar |
| `/web/` com Basic | engine, 200 + cookie |
| `GET /api/v1/workspaces` só cookie | engine, 200 |
| `POST /admin/commit` só cookie | 401 |
| `GET /api/v1/workspaces` com bearer | engine, 200 |
| `GET` sem cookie e sem bearer | 401 |

O log do sidecar mostrou subrequest de `/verify` **só** para `/admin/commit` e
`/api/v1/workspaces`. A suíte `e2e/live.spec.ts` passou inteira (4/4, incluindo
"sessão só-cookie vê as telas e não oferece mutação") apontada para a porta do
Caddy em vez do engine.

Antes do corte, repetir contra a porta nova:

1. os cinco headers forjados (um a um e combinados) chegam só com valor
   verificado;
2. um `/hook` com `X-Memory-Actor-Session-Id` preserva a sessão;
3. `/keys*`, `/web*` e `/.well-known/oauth-protected-resource` não aparecem no
   log do `mcp-auth` como subrequest de `/verify` — se aparecerem, o catch-all
   deixou de ser mutuamente exclusivo;
4. `/.well-known/oauth-protected-resource` responde metadata (vem do sidecar),
   não 404 (que seria o engine);
5. Basic no `/web` → `GET /api/v1/...` só com cookie devolve 200 e
   `POST /admin/...` só com cookie devolve 401;
6. `E2E_BASE_URL=http://127.0.0.1:8080/web npx playwright test e2e/live.spec.ts`
   passa 4/4.

O volume nomeado pode entrar vazio: a imagem provisiona `/data` já com dono
`65532:65532`, e um volume novo herda dono e modo do diretório que existe na
imagem naquele caminho. **Não** troque o `USER` para root nem adicione um passo
de `chown` — sem o `/data` na imagem, o Docker cria o volume como `root:root`
`0755`, o processo (UID 65532) não consegue criar o `keys.db` e o container
morre no boot com `keys_db_open_failed: unable to open database file`.
Reproduzido com volume limpo e corrigido na imagem antes do rollout.

Na borda (o proxy que já termina TLS) — **contrato exercitado localmente, não
suposto**:

1. `/{basePath}/keys` e `/{basePath}/keys/*` vão **direto** para o `mcp-auth`.
   Esse caminho **não** pode passar pelo `forwardAuth` do próprio sidecar: ele
   *é* o serviço de auth, e mandá-lo autenticar a si mesmo devolve 401.
2. `/{basePath}/web*` (documento + assets) **não** passa pelo `forwardAuth`:
   quem autentica a UI é o **próprio engine** (Basic com a senha = bearer raiz,
   depois cookie `ai_memory_auth`). Não há oauth2-proxy nesta topologia. Com o
   forwardAuth ligado ali, o browser manda `Authorization: Basic …`, o sidecar
   não entende e a tela fica em branco com `forwardAuth denied: 401`.
3. O resto (`/mcp`, `/api/v1`, `/admin`, `/hook`, `/handoff`) passa pelo
   `forwardAuth` do `/verify`, que **substitui** o `Authorization` pelo
   `ACTOR_PROXY_BEARER_TOKEN` e injeta os headers de ator. Header acumulado faz
   o engine responder `400 Ambiguous`; par OIDC incompleto responde `400`.

   A borda descarta os **cinco** headers de ator que o sidecar emite —
   enumerados, dentro de um `route`, como na config acima. **Não** descarte
   "qualquer `X-Memory-Actor-*`": o `X-Memory-Actor-Session-Id` é legítimo
   (vem do hook e do `mcp_bridge`, o sidecar nunca o emite) e um strip curinga
   o apaga. Ver a seção da config para o porquê e os testes.

### Credencial do operador no browser (o detalhe que decide se Consumidores funciona)

A SPA guarda **uma** chave e a manda como Bearer para tudo. Para que a mesma
chave sirva ao engine *e* ao `/keys`, ela precisa ser uma chave `amk_` com
escopo `admin` cujo **owner seja `kind=subject`** com `issuer`/`subject` iguais
ao `root_issuer`/`root_subject` configurados no engine. Nesse caso, verificado
ponta a ponta:

| Destino | Resultado |
|---|---|
| `GET /keys/whoami` | `can_issue: true`, identidade `kind=subject` |
| `GET /api/v1/*` | 200 |
| `GET /admin/status` | 200 (Root pelo par OIDC no rung de proxy confiável) |
| `GET /admin/users` | 200 (UserManagement, root-only) |

Se o owner for `kind=user`, o sidecar (corretamente) **não** emite o par
issuer/sub, o engine concede apenas `User` e `/admin/*` responde 403 — a tela
administrativa desaparece. Uma chave de operador com owner `user` serve para
gerir chaves, não para administrar o engine.

Alternativa sem chave de operador: a borda encaminha o **access token OIDC** do
operador para `/keys*` (o sidecar valida o JWT e exige a realm role
`mcp:admin`), e o operador cola o bearer raiz do engine na UI para as chamadas
administrativas. São duas credenciais em vez de uma — funciona, mas o rodapé da
sidebar passa a mostrar a chave do engine, não a identidade OIDC.

### Bootstrap da primeira chave

A emissão é **fail-closed**: sem identidade, `POST /keys` responde 403 e a UI
desabilita o submit com o estado `AUSENTE`. Então a primeira chave `admin` entra
direto no banco:

```bash
SECRET="amk_$(openssl rand -hex 20)"
SHA=$(printf '%s' "$SECRET" | shasum -a 256 | cut -d' ' -f1)
sqlite3 /data/keys.db "INSERT INTO consumer_keys
 (id,key_sha256,key_last4,actor_user,scopes,owner_kind,owner_user,owner_issuer,owner_subject,owner_label,created_at)
 VALUES ('operator','$SHA','${SECRET: -4}','<seu-usuario>','read,write,admin',
         'subject',NULL,'<root_issuer>','<root_subject>','<seu-usuario>',$(date +%s));"
echo "$SECRET"   # cole na tela de login; é a única vez que aparece
```

Papel no Keycloak: criar a realm role `mcp:admin` para quem pode emitir chaves.
Se o realm não puder mudar, usar `KEYS_ADMIN_SUBJECTS` (`issuer|subject`).

## 4. Migrar consumidores (ordem decidida: CLIs antes do hook)

Para cada CLI (claude-code, cursor, codex, omp), um a um:

1. Emitir chave `read,write` na tela **Consumidores** (o campo *Responsável* é
   capturado da sessão de quem emite — não é digitável; sem identidade o submit
   fica travado, por construção).
2. Trocar o token no cliente, exercitar uma leitura e uma escrita.
3. Confirmar a atribuição: a escrita deve aparecer com o `actor_user` da chave.

Só depois de todos os CLIs migrados: rotacionar `AI_MEMORY_AUTH_TOKEN` (o
bearer raiz deixa de ser credencial de cliente e volta a ser quebra-galho de
operador). O token de hook é o **último** a sair, porque o `/hook` é o caminho
que não pode piscar.

## 5. Verificação de borda

```bash
B=https://memory.djalmajr.dev

# chave nova: 200 e escrita atribuída
curl -s -o /dev/null -w '%{http_code}\n' -H "Authorization: Bearer amk_..." $B/api/v1/workspaces

# chave revogada: 401
curl -s -o /dev/null -w '%{http_code}\n' -H "Authorization: Bearer amk_<revogada>" $B/api/v1/workspaces

# ator forjado pelo cliente: o engine recusa (400) ou ignora — nunca aceita
curl -s -o /dev/null -w '%{http_code}\n' \
  -H "Authorization: Bearer amk_..." -H 'X-Memory-Actor-User: fulano' $B/api/v1/workspaces
```

## 6. Smoke da área administrativa em produção

A suíte `e2e/live.spec.ts` é opt-in e não guarda credencial nenhuma no arquivo:

```bash
cd ~/Developer/djalmajr/ai-memory-ui
E2E_BASE_URL=https://memory.djalmajr.dev/web \
E2E_ADMIN_TOKEN=<bearer raiz> \
E2E_USER_TOKEN=<token de usuário do banco> \
npx playwright test e2e/live.spec.ts
```

Cobre: telas administrativas com dados reais, escopo listando páginas e abrindo
o leitor, tier de usuário sem área administrativa, e — importante — **sessão
só-cookie não recebe botão de mutação**.

## 7. Nota de auth que muda a percepção da UI

Num engine com auth configurada, o próprio HTML do `/web` é protegido. Quem
autentica é o engine: Basic com **qualquer** usuário e a senha = bearer raiz.
Aceito o Basic, ele emite o cookie `ai_memory_auth`, e **esse cookie autentica
apenas GET** — toda mutação exige o header `Authorization`.

A SPA trata isso explicitamente: uma sessão autenticada só por cookie recebe o
degrau `cookie-admin`, vê todas as telas de leitura e tem **todas as ações de
escrita desabilitadas**, com o aviso "cole sua chave de acesso para executar
operações". Chamar essa sessão de "anônima" seria mentira, e oferecer os botões
seria oferecer um 401 garantido.
