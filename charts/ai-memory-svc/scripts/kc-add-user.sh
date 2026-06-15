#!/usr/bin/env sh
# Idempotent: add a developer to the ai-memory realm group (the group that
# carries the mcp:read / mcp:write roles), so mcp-auth stops returning
# `403 missing_role` for them. GENERIC - every instance-specific value is an env
# var; nothing is hardcoded, so the same script works against any realm/host.
#
# Run AFTER kc-bootstrap.sh (the group must already exist). This is a per-DEV
# imperative action - there is intentionally NO Helm Job for it (a Job templated
# with one username would mean a helm upgrade per developer). Two ways to run it,
# mirroring kc-bootstrap.sh:
#   A) By hand, INSIDE the Keycloak pod (admin creds come from the pod env, so
#      no secret leaves the cluster):
#        POD=$(kubectl -n <ns> get pods -l app=keycloak -o name | head -1)
#        kubectl -n <ns> exec -i "$POD" -- sh -c \
#          'KC_REALM=<realm> DEV_USERNAME=<username> \
#           KC_SERVER=http://localhost:8080/auth \
#           KC_ADMIN_USER="$KEYCLOAK_ADMIN" KC_ADMIN_PASS="$KEYCLOAK_ADMIN_PASSWORD" sh -s' \
#          < charts/ai-memory-svc/scripts/kc-add-user.sh
#   B) Windows admins: pipe with Get-Content (see KEYCLOAK-BOOTSTRAP.md).
#
# Config (env):
#   KC_SERVER     kcadm base URL incl. any legacy /auth suffix (default http://localhost:8080)
#   KC_REALM      target realm                                  (required)
#   DEV_USERNAME  the developer's Keycloak username             (required)
#   KC_ADMIN_USER master-realm admin user                       (required)
#   KC_ADMIN_PASS master-realm admin password                   (required)
#   GROUP         realm group that carries the roles  (default ai-memory)
#   KCADM         path to kcadm.sh (default /opt/keycloak/bin/kcadm.sh)
set -eu

KCADM="${KCADM:-/opt/keycloak/bin/kcadm.sh}"
KC_SERVER="${KC_SERVER:-http://localhost:8080}"
GROUP="${GROUP:-ai-memory}"
: "${KC_REALM:?set KC_REALM}"
: "${DEV_USERNAME:?set DEV_USERNAME (the developer Keycloak username)}"
: "${KC_ADMIN_USER:?set KC_ADMIN_USER}"
: "${KC_ADMIN_PASS:?set KC_ADMIN_PASS}"

"$KCADM" config credentials --server "$KC_SERVER" --realm master \
  --user "$KC_ADMIN_USER" --password "$KC_ADMIN_PASS" >/dev/null

# kcadm pretty-prints one field per line; this anchored extractor only matches an
# `id` field at the START of a line, so it can never pick up a nested or inline id
# even if a future kcadm emitted compact JSON.
id_of() { sed -n 's/^[[:space:]]*"id"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' | head -1; }

# Resolve the user id by EXACT username (usernames are unique per realm). A failed
# kcadm here leaves the var empty (assignment from a failed substitution does not
# trip `set -e`), which the guard below turns into a loud exit - config
# credentials above already failed fast on bad creds.
USER_ID="$("$KCADM" get users -r "$KC_REALM" -q username="$DEV_USERNAME" -q exact=true --fields id 2>/dev/null | id_of)"
[ -n "$USER_ID" ] || { echo "user '$DEV_USERNAME' not found in realm '$KC_REALM'" >&2; exit 1; }

# Resolve the group id, then RE-VALIDATE that the id really names $GROUP exactly.
# `exact=true` is honoured on Keycloak 23+, but a build that ignores it degrades
# `search` to a substring match (e.g. `ai-memory` would also return
# `ai-memory-readonly`). The re-fetch-by-id name check refuses a wrong match
# BEFORE we mutate anything, so we can never grant via the wrong group.
GROUP_ID="$("$KCADM" get groups -r "$KC_REALM" -q search="$GROUP" -q exact=true --fields id,name 2>/dev/null | id_of)"
[ -n "$GROUP_ID" ] || { echo "group '$GROUP' not found in realm '$KC_REALM' (run kc-bootstrap.sh first)" >&2; exit 1; }
GROUP_NAME="$("$KCADM" get "groups/$GROUP_ID" -r "$KC_REALM" --fields name 2>/dev/null \
  | sed -n 's/^[[:space:]]*"name"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' | head -1)"
[ "$GROUP_NAME" = "$GROUP" ] || { echo "resolved group id $GROUP_ID names '$GROUP_NAME', not '$GROUP'; refusing" >&2; exit 1; }

# PUT the membership. `-n` = no-merge (PUT with no preceding GET; the
# users/{id}/groups/{gid} endpoint has no GET). The PUT is idempotent server-side,
# so re-adding an existing member returns 204 with no error; a genuine failure
# trips `set -e` here.
"$KCADM" update "users/$USER_ID/groups/$GROUP_ID" -r "$KC_REALM" \
  -s realm="$KC_REALM" -s userId="$USER_ID" -s groupId="$GROUP_ID" -n

# Assert the REAL post-condition: the developer now has the mcp roles IN EFFECT.
# Effective roles include group-inherited ones, so this confirms the grant
# actually conveys access - a group that exists but lost its role mapping fails
# here instead of reporting a hollow "added to group" success.
EFFECTIVE="$("$KCADM" get-roles -r "$KC_REALM" --uusername "$DEV_USERNAME" --effective 2>/dev/null)"
if printf '%s' "$EFFECTIVE" | grep -q "\"name\" : \"mcp:read\"" \
  && printf '%s' "$EFFECTIVE" | grep -q "\"name\" : \"mcp:write\""; then
  echo "ok: '$DEV_USERNAME' has mcp:read + mcp:write in effect (via group '$GROUP')"
else
  echo "FAILED: '$DEV_USERNAME' lacks mcp:read/mcp:write after update - is '$GROUP' mapped to the roles? (run kc-bootstrap.sh)" >&2
  exit 1
fi
