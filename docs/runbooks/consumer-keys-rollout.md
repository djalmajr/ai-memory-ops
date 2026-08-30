# Runbook — área administrativa + chaves por consumidor

Rollout da SPA administrativa (`ai-memory-ui`) e do subsistema de chaves por
consumidor do `mcp-auth` (issue #9) no deploy pessoal (Hetzner).

O compose de produção **não vive neste repositório** — ele está no servidor, em
`/opt/ai-memory/compose.yml`. Este runbook descreve as mudanças a aplicar lá.

## 0. Estado (atualizado 2026-08-30)

**Aplicado em produção:**

- SPA administrativa em `https://memory.djalmajr.dev/web/`, servida pelo
  engine 1.32.2. O documento e os assets são públicos na borda para que a rota
  `/login` carregue; administração, leitura e mutações continuam protegidas por
  chave Bearer.
- `mcp-auth` em modo `keys-only`, com banco no volume nomeado
  `ai-memory_mcp-auth-keys`; `/keys*` é roteado diretamente ao sidecar.
- Caddy em `127.0.0.1:8080` faz o `forward_auth` do restante da borda. O túnel
  Cloudflare aponta para essa porta; rollback do ingresso = reapontar para
  `http://127.0.0.1:49374`.
- Chave `operator` (`read,write,admin`, owner `subject`) e chaves dedicadas
  `claude-code`, `cursor`, `codex` e `omp` (`read,write`) emitidas. Cada cliente
  leu, escreveu e apagou uma página com atribuição `djalmajr`.
- Os quatro CLIs usam somente suas chaves `amk_`. Claude Code e OMP também
  receberam hooks com credencial dedicada.
- `AI_MEMORY_AUTH_TOKEN`, `ACTOR_PROXY_BEARER_TOKEN` e `HOOK_AUTH_TOKEN` foram
  rotacionados e são distintos. `PASSTHROUGH_UNKNOWN_BEARER=0`; o bearer raiz
  anterior e bearers desconhecidos respondem 401 na borda.
- O spool local legado, que havia atingido 10.000 eventos sem autenticação, foi
  autenticado, replayado e drenado antes da rotação. Sete `PreCompact` presos
  foram recuperados com o provider LLM temporariamente desativado, evitando o
  timeout de 300 s por evento; a configuração normal foi restaurada em seguida.

Backups operacionais: `/opt/ai-memory/compose.yml.bak-pre-consumer-keys`,
`/opt/ai-memory/Caddyfile.bak-pre-spool-drain`, backups de rotação
`/opt/ai-memory/{.env,compose.yml}.bak-token-rotation-*` e o par
`/opt/ai-memory/{Caddyfile,compose.yml}.bak-login-route-*`.

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
print("caddy web == root     :", "MATCH" if cw and cw==rt else "DIFFER/AUSENTE")
'
```

Lê o compose **resolvido**, então cobre literal e `${...}` igualmente, e não
depende de o `.env` estar exportado no shell. O estado esperado é
`engine == sidecar: MATCH`, `proxy != root: DISTINCT` e
`caddy web == root: MATCH`.

`DIFFER` em `engine == sidecar` faz toda chave `amk_` retornar 401; `SAME` em
`proxy != root` transforma toda identidade traduzida em Root sem atribuição.
`DIFFER/AUSENTE` no Caddy devolve o desafio Basic antes de a SPA carregar.

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
> Portanto o passo 3 exige uma decisão de infraestrutura ainda não tomada:
> introduzir um proxy local na frente de `engine` + `mcp-auth` e reapontar o
> `cloudflared` para ele. Isso muda o único caminho de entrada do sistema em
> produção — deve ser feito com janela e rollback combinados, não de passagem.

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

### Proxy sugerido: Caddy — config **testada**, não proposta

O container recebe somente a credencial interna necessária para servir a SPA;
não use `env_file`, que exporia ao Caddy todas as credenciais de providers:

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
`/verify`; só `/mcp` e `/api/v1/workspaces` chamaram.

Consequência: `/keys*` e `/web*` não passam pelo strip do catch-all. Isso é
inerte nos dois destinos:

- o `mcp-auth` nunca lê `X-Memory-Actor-*` de entrada; o dono da chave vem
  sempre da credencial de quem chama;
- em `/web*`, o Caddy **sobrescreve** `Authorization` com o bearer raiz apenas
  na conexão interna ao engine. O engine ignora headers de ator no degrau raiz
  (`actor_headers_are_ignored_on_the_root_rung`) e não emite
  `ai_memory_auth`, pois esse cookie só nasce de Basic. O bearer não é enviado
  ao browser; o cliente recebe somente HTML/assets públicos e precisa colar uma
  chave na rota `/login` para chamar qualquer API.

Corte com risco baixo: subir Caddy + mcp-auth numa porta paralela
(`127.0.0.1:8080`) com o engine ainda publicando 49374, validar por curl e pela
suíte `live.spec.ts` apontada para `http://127.0.0.1:8080/web`, e só então
reapontar o `cloudflared`. Rollback = reapontar o túnel de volta.

Esta config foi exercitada com a cadeia inteira — Caddy 2 + `mcp-auth` +
engine 1.32.2 real servindo a SPA. Resultado:

| Request | Quem respondeu |
|---|---|
| `/.well-known/oauth-protected-resource` | sidecar, metadata/404 próprio |
| `/keys` sem credencial | sidecar |
| `/web/` sem credencial | engine, 200 via bearer interno; sem cookie/challenge |
| `/web/ops` sem credencial | SPA redireciona para `/web/login` |
| `GET /api/v1/workspaces` sem bearer | 401 |
| `GET /api/v1/workspaces` com bearer válido | engine, 200 |

O log do sidecar mostra subrequest de `/verify` apenas para as APIs protegidas.
A suíte `e2e/live.spec.ts` passou 4/4 contra a borda pública, incluindo login
sem chave e entrada pela tela com a chave `operator`.

Antes de qualquer novo corte, repetir:

1. os cinco headers forjados (um a um e combinados) chegam só com valor
   verificado;
2. um `/hook` com `X-Memory-Actor-Session-Id` preserva a sessão;
3. `/keys*`, `/web*` e `/.well-known/oauth-protected-resource` não aparecem no
   log do `mcp-auth` como subrequest de `/verify`;
4. `/.well-known/oauth-protected-resource` vem do sidecar. Com OAuth desligado
   devolve `404 page not found`; com `OAUTH_ENABLED=true`, vira 200;
5. `POST /hook` com o token de hook devolve **202** e a sessão registra
   `actor_user`;
6. `/web/` sem credencial devolve 200, sem `WWW-Authenticate` e sem
   `Set-Cookie`; `/api/v1/workspaces` sem bearer continua 401;
7. `E2E_BASE_URL=http://127.0.0.1:8080/web npx playwright test e2e/live.spec.ts`
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

A alternativa de encaminhar o **access token OIDC** do operador para `/keys*`
(sidecar valida o JWT e exige a realm role `mcp:admin`) **não existe neste
host**: sem provedor no ar não há JWT para validar, e em `mode: keys-only` o
ramo JWT falha fechado por construção. Ela volta a valer se o Keycloak for
implantado. Até então, a credencial do operador é uma chave `amk_` com escopo
`admin` — e o `owner_kind` dela decide se abre `Usuários` (ver os caminhos A e B
no bootstrap abaixo).

### Bootstrap da primeira chave

A emissão é **fail-closed**: sem identidade, `POST /keys` responde 403 e a UI
desabilita o submit com o estado `AUSENTE`. Então a primeira chave `admin` entra
direto no banco:

A imagem é `scratch`: **não há shell, `apk` nem `sqlite3` dentro dela** — logo
nada de `docker compose exec mcp-auth sh`. Suba o sidecar uma vez (ele cria o
schema), pare, semeie por um container Alpine que compartilha o **volume
nomeado**, e suba de novo. Não use o `sqlite3` do host contra bind mount com o
sidecar aberto: o WAL sobre bind mount do macOS devolve
`disk I/O error (1034)` (reproduzido).

O `owner_kind` da chave do operador decide se **uma** chave serve para tudo. O
par `issuer|subject` é só um par de strings que engine e sidecar combinam —
**não** exige provedor OIDC no ar:

**Caminho A — uma chave para tudo (recomendado).** Configure no engine
`root_issuer` + `root_subject` (hoje **ausentes** em produção; `root_username`
já está setado) e emita a chave com `owner_kind='subject'` casando esses
valores. O engine reconhece o par como raiz, então a mesma chave gere as chaves
no sidecar **e** abre `Usuários` (UserManagement é root-only). Exige um restart
do engine.

**Caminho B — sem mexer no engine.** `owner_kind='user'`. A chave gere as chaves
de consumidor e escreve na memória com atribuição, mas **não** é raiz: a tela
`Usuários` responde 403. O bearer raiz do engine abriria `Usuários`, mas ele não
é aceito em `/keys` (fail-closed, 403) — e a SPA guarda **uma** chave só, então
é escolher uma das duas telas. Por isso A é o recomendado.

```bash
SECRET="amk_$(openssl rand -hex 20)"
SHA=$(printf %s "$SECRET" | sha256sum | cut -d' ' -f1)
VOL=$(docker volume ls -q | grep mcp-auth-keys)   # confirme o nome do volume

docker compose stop mcp-auth
docker run --rm -v "$VOL":/data alpine sh -c \
  "apk add --no-cache sqlite >/dev/null && sqlite3 /data/keys.db \"INSERT INTO consumer_keys
   (id,key_sha256,key_last4,actor_user,scopes,owner_kind,owner_user,owner_issuer,owner_subject,owner_label,created_at)
   VALUES ('operator','$SHA','${SECRET: -4}','<seu-usuario>','read,write,admin',
           'subject',NULL,'<root_issuer>','<root_subject>','<seu-usuario>',$(date +%s));\""
docker compose start mcp-auth

echo "$SECRET"   # cole na tela de login; é a única vez que aparece
```

Acima é o **caminho A**. Para o B, troque as cinco colunas de dono por
`'user','<seu-usuario>',NULL,NULL,'<seu-usuario>'`.

Confira: `curl -H "Authorization: Bearer $SECRET" <base>/keys/whoami` →
`{"can_issue":true,"identity":{...}}`.

O papel `mcp:admin` e o `KEYS_ADMIN_SUBJECTS` **não se aplicam** neste host:
ambos casam `issuer|subject` de um **JWT validado**, e sem provedor não há JWT.
Uma chave `amk_` com escopo `admin` é o caminho de emissão aqui. Voltam a valer
se o Keycloak for implantado (receita em `djalmajr/infra` →
`runbooks/05-instalar-stack.md`, hoje "não implantada").

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

### Por que o `/hook` não pode piscar

Registrado em `djalmajr/infra` →
`gotchas/hook-auth-personal-is-oidc-device-not-static.md`: quando o hook 401a, o
`<data_dir>/hook-spool` enche até o cap (10000 arquivos) com capturas reais
presas, e o backlog morto **bloqueia a fila** — o drain é oldest-first, então
captura nova não passa até alguém descartar o backlog. Não é perder só os
eventos da janela: a captura para.

Naquele episódio o `mcp-auth` era Keycloak-only e recusava token estático no
`/hook`. O sidecar atual tem o atalho do `HOOK_AUTH_TOKEN` (`isHookPath` +
comparação de tempo constante) e foi medido devolvendo **202** com `actor_user`
gravado — o caminho estático segue válido aqui. Mesmo assim: teste `POST /hook`
**antes** de trocar qualquer credencial de hook e, se algo 401ar, esvazie o
spool antes de seguir.

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

A suíte `e2e/live.spec.ts` é opt-in e não guarda credencial nenhuma no arquivo.
Na borda com a SPA pública, a chave administrativa entra apenas no
`localStorage` do browser de teste:

```bash
cd ~/Developer/djalmajr/ai-memory-ui
E2E_BASE_URL=https://memory.djalmajr.dev/web \
E2E_ADMIN_TOKEN=<chave amk_ operator> \
E2E_USER_TOKEN=<chave amk_ read,write> \
E2E_SCOPE_PATH=/s/djalmajr/ai-memory \
npx playwright test e2e/live.spec.ts
```

Cobre: login prototipado sem chave, telas administrativas com dados reais,
escopo listando páginas e abrindo o leitor, e tier de usuário sem área
administrativa.

## 7. Contrato de autenticação da UI

`/web*` é público **somente para carregar a SPA**. O Caddy autentica essa rota
internamente no engine com `AI_MEMORY_WEB_UPSTREAM_TOKEN`, cujo valor vem de
`${AI_MEMORY_AUTH_TOKEN}` no compose. O browser nunca recebe o token raiz nem
um cookie root.

Sem chave no `localStorage`, os probes de `/api/v1` respondem 401 e o guard
leva para `/login`. A chave colada fica apenas no navegador e é enviada como
Bearer nas APIs. Sair remove essa chave e volta ao login. `/keys*`, `/api/v1`,
`/admin`, `/mcp`, `/hook` e `/handoff` continuam protegidos.
