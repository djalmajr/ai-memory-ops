#!/usr/bin/env sh
# Idempotent Keycloak realm bootstrap for an ai-memory-svc instance: creates the
# three OIDC clients + the mcp:read / mcp:write realm roles + a group that
# carries them. GENERIC — every instance-specific value is an env var; nothing
# is hardcoded, so the same script provisions any realm/host.
#
# Run ONCE per realm, AFTER the realm exists. Two supported ways:
#   A) By hand, INSIDE the Keycloak pod (admin creds come from the pod env):
#        kubectl exec -i <keycloak-pod> -- sh -c \
#          'KC_REALM=<realm> PUBLIC_URL=https://host[/base] \
#           KC_SERVER=http://localhost:8080/auth \
#           KC_ADMIN_USER="$KEYCLOAK_ADMIN" KC_ADMIN_PASS="$KEYCLOAK_ADMIN_PASSWORD" sh -s' \
#          < kc-bootstrap.sh
#      (Drop the `/auth` from KC_SERVER on Keycloak builds with no relative path —
#       a `404 Not Found` at login means flip it.)
#   B) As the optional post-install Helm hook Job (set keycloakBootstrap.enabled=true).
#
# Clients created (idempotent):
#   ${OAUTH2_CLIENT_ID}  confidential, standard flow, PKCE S256  -> oauth2-proxy /web login
#   ${MCP_CLIENT_ID}     public, standard flow + DCR, PKCE S256  -> MCP browser login
#   ${DEVICE_CLIENT_ID}  public, DEVICE-FLOW only, no PKCE       -> CLI / headless hooks
#                        (the CLI device flow sends no PKCE, so this client must
#                         NOT enforce it — that is why it is separate from the MCP one)
#
# Config (env):
#   KC_SERVER        kcadm base URL incl. any legacy /auth suffix (default http://localhost:8080)
#   KC_REALM         target realm                                  (required)
#   PUBLIC_URL       public base URL incl. base path, no trailing / (required)
#                    e.g. https://host  or  https://host/wiki
#   KC_ADMIN_USER    master-realm admin user                       (required)
#   KC_ADMIN_PASS    master-realm admin password                   (required)
#   GROUP            realm group that carries the roles  (default ai-memory)
#   OAUTH2_CLIENT_ID (default ai-memory-oauth2-proxy)
#   MCP_CLIENT_ID    (default ai-memory-mcp)
#   DEVICE_CLIENT_ID (default ai-memory-cli)
#   KCADM            path to kcadm.sh (default /opt/keycloak/bin/kcadm.sh)
set -eu

KCADM="${KCADM:-/opt/keycloak/bin/kcadm.sh}"
KC_SERVER="${KC_SERVER:-http://localhost:8080}"
GROUP="${GROUP:-ai-memory}"
OAUTH2_CLIENT_ID="${OAUTH2_CLIENT_ID:-ai-memory-oauth2-proxy}"
MCP_CLIENT_ID="${MCP_CLIENT_ID:-ai-memory-mcp}"
DEVICE_CLIENT_ID="${DEVICE_CLIENT_ID:-ai-memory-cli}"
: "${KC_REALM:?set KC_REALM}"
: "${PUBLIC_URL:?set PUBLIC_URL (e.g. https://host[/base], no trailing slash)}"
: "${KC_ADMIN_USER:?set KC_ADMIN_USER}"
: "${KC_ADMIN_PASS:?set KC_ADMIN_PASS}"

PUBLIC_URL="${PUBLIC_URL%/}"                                   # strip trailing slash
ORIGIN="$(printf '%s' "$PUBLIC_URL" | sed -E 's#^(https?://[^/]+).*#\1#')"

"$KCADM" config credentials --server "$KC_SERVER" --realm master \
  --user "$KC_ADMIN_USER" --password "$KC_ADMIN_PASS" >/dev/null

client_exists() {
  "$KCADM" get clients -r "$KC_REALM" -q clientId="$1" 2>/dev/null \
    | grep -q "\"clientId\" : \"$1\""
}

# oauth2-proxy: confidential, standard flow, PKCE (browser /web login)
if client_exists "$OAUTH2_CLIENT_ID"; then
  echo "$OAUTH2_CLIENT_ID: exists"
else
  "$KCADM" create clients -r "$KC_REALM" \
    -s clientId="$OAUTH2_CLIENT_ID" -s enabled=true -s protocol=openid-connect \
    -s publicClient=false -s standardFlowEnabled=true -s directAccessGrantsEnabled=false \
    -s "redirectUris=[\"$PUBLIC_URL/oauth2/callback\"]" \
    -s "webOrigins=[\"$ORIGIN\"]" \
    -s 'attributes={"pkce.code.challenge.method":"S256"}' >/dev/null
  echo "$OAUTH2_CLIENT_ID: created"
fi

# mcp: public, standard flow + DCR, PKCE (MCP browser login)
if client_exists "$MCP_CLIENT_ID"; then
  echo "$MCP_CLIENT_ID: exists"
else
  "$KCADM" create clients -r "$KC_REALM" \
    -s clientId="$MCP_CLIENT_ID" -s enabled=true -s protocol=openid-connect \
    -s publicClient=true -s standardFlowEnabled=true \
    -s "redirectUris=[\"http://localhost:*\",\"$PUBLIC_URL/*\"]" \
    -s 'attributes={"pkce.code.challenge.method":"S256"}' >/dev/null
  echo "$MCP_CLIENT_ID: created"
fi

# device client: public, device-flow only, NO PKCE (CLI / headless hooks)
if client_exists "$DEVICE_CLIENT_ID"; then
  echo "$DEVICE_CLIENT_ID: exists"
else
  "$KCADM" create clients -r "$KC_REALM" \
    -s clientId="$DEVICE_CLIENT_ID" -s enabled=true -s protocol=openid-connect \
    -s publicClient=true \
    -s standardFlowEnabled=false -s implicitFlowEnabled=false \
    -s directAccessGrantsEnabled=false -s serviceAccountsEnabled=false \
    -s 'attributes={"oauth2.device.authorization.grant.enabled":"true"}' >/dev/null
  echo "$DEVICE_CLIENT_ID: created (device-flow, no PKCE)"
fi

# realm roles mcp:read / mcp:write. The create is the elif condition (exempt
# from `set -e`), so a real failure falls through to the loud `else` + exit.
for r in mcp:read mcp:write; do
  if "$KCADM" get roles -r "$KC_REALM" 2>/dev/null | grep -q "\"name\" : \"$r\""; then
    echo "role $r: exists"
  elif "$KCADM" create roles -r "$KC_REALM" -s name="$r" >/dev/null; then
    echo "role $r: created"
  else
    echo "role $r: FAILED to create" >&2; exit 1
  fi
done

# group that carries the roles
if "$KCADM" get groups -r "$KC_REALM" -q search="$GROUP" --fields name 2>/dev/null \
  | grep -q "\"name\" : \"$GROUP\""; then
  echo "group $GROUP: exists"
elif "$KCADM" create groups -r "$KC_REALM" -s name="$GROUP" >/dev/null; then
  echo "group $GROUP: created"
else
  echo "group $GROUP: FAILED to create" >&2; exit 1
fi

# Map each role to the group, then VERIFY by name. add-roles exits non-zero on
# an already-mapped role, so we ignore its status and assert the post-condition
# (the role IS in the group's realm role-mappings) — a genuine miss fails loud.
for r in mcp:read mcp:write; do
  "$KCADM" add-roles -r "$KC_REALM" --gname "$GROUP" --rolename "$r" >/dev/null 2>&1 || true
  if "$KCADM" get-roles -r "$KC_REALM" --gname "$GROUP" 2>/dev/null | grep -q "\"name\" : \"$r\""; then
    echo "role $r -> group $GROUP: ok"
  else
    echo "role $r -> group $GROUP: FAILED (not mapped)" >&2; exit 1
  fi
done

echo
echo "Done in realm '$KC_REALM'. Clients: $OAUTH2_CLIENT_ID, $MCP_CLIENT_ID, $DEVICE_CLIENT_ID."
echo "Add each developer to group '$GROUP' (or grant mcp:read/mcp:write directly)."
