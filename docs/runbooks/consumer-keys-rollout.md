# Runbook — área administrativa + chaves por consumidor

Rollout da SPA administrativa (`ai-memory-ui`) e do subsistema de chaves por
consumidor do `mcp-auth` (issue #9) no deploy pessoal (Hetzner).

O compose de produção **não vive neste repositório** — ele está no servidor, em
`/opt/ai-memory/compose.yml`. Este runbook descreve as mudanças a aplicar lá.

## 0. Estado de partida (verificado)

- `/keys*` **não é roteável hoje** por nenhuma config versionada: o mux do
  `mcp-auth` não tinha as rotas, o helm não mapeia o caminho, e o compose é
  server-only. O passo 3 é o que fecha isso.
- A SPA já degrada sozinha: sem backend de chaves, a tela **Consumidores**
  mostra o banner "backend indisponível" e inventário vazio — nunca linhas
  fabricadas.
- `ACTOR_PROXY_BEARER_TOKEN` já está ativo em produção (sondado: `400
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

## 2. Publicar `dist/` e **reiniciar o engine**

O engine serve a SPA com `--web-ui-dir <dir>` e **lê o `index.html` uma vez, no
boot**. Publicar um build novo sem reiniciar deixa o HTML antigo em memória
apontando para assets com hash que não existem mais; o browser recebe o
fallback HTML no lugar do módulo e a tela fica **branca** com
`Failed to load module script: ... MIME type of "text/html"`.

> **Ordem obrigatória: publicar `dist/` → reiniciar/recriar o container do
> engine.** Isto foi reproduzido duas vezes na validação local.

```bash
rsync -a --delete dist/ root@<host>:/opt/ai-memory/web-ui/
ssh root@<host> 'cd /opt/ai-memory && docker compose restart ai-memory'
```

Verificação: o `index.html` servido deve referenciar o mesmo hash que está em
disco.

```bash
curl -s -u ":$ROOT_TOKEN" https://memory.djalmajr.dev/web/ | grep -o 'assets/[^"]*\.js'
ls /opt/ai-memory/web-ui/assets/ | grep index
```

## 3. Subir o `mcp-auth` com chaves e rotear `/keys*`

Imagem nova (multi-arch, conforme `images/mcp-auth/README.md`), então no
compose do servidor:

```yaml
  mcp-auth:
    image: <registry>/mcp-auth:<tag-nova>
    environment:
      OIDC_ISSUER: https://<keycloak>/realms/memory
      KEYS_DB: /data/keys.db
      # MESMO valor já configurado no engine — é o que o /verify injeta upstream.
      ACTOR_PROXY_BEARER_TOKEN: ${ACTOR_PROXY_BEARER_TOKEN}
      # Mantém os CLIs atuais funcionando durante a migração (passo 4).
      PASSTHROUGH_UNKNOWN_BEARER: "1"
      HOOK_AUTH_TOKEN: ${HOOK_AUTH_TOKEN}
    volumes:
      - mcp-auth-keys:/data
```

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
2. `/{basePath}/web*` (documento + assets) **não** passa pelo `forwardAuth`: a
   sessão da UI é do oauth2-proxy. Com o forwardAuth ligado ali, o browser manda
   `Authorization: Basic …`, o sidecar não entende e a tela fica em branco com
   `forwardAuth denied: 401`.
3. O resto (`/mcp`, `/api/v1`, `/admin`, `/hook`, `/handoff`) passa pelo
   `forwardAuth` do `/verify`, que **substitui** o `Authorization` pelo
   `ACTOR_PROXY_BEARER_TOKEN` e injeta os headers de ator. A borda deve
   **descartar** qualquer `X-Memory-Actor-*` vindo do cliente antes de chamar o
   `/verify` — header acumulado faz o engine responder `400 Ambiguous`; par OIDC
   incompleto responde `400`.

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

Num engine com auth configurada, o próprio HTML do `/web` é protegido. Depois do
Basic (ou do oauth2-proxy) o engine emite o cookie `ai_memory_auth`, e **esse
cookie autentica apenas GET** — toda mutação exige o header `Authorization`.

A SPA trata isso explicitamente: uma sessão autenticada só por cookie recebe o
degrau `cookie-admin`, vê todas as telas de leitura e tem **todas as ações de
escrita desabilitadas**, com o aviso "cole sua chave de acesso para executar
operações". Chamar essa sessão de "anônima" seria mentira, e oferecer os botões
seria oferecer um 401 garantido.
