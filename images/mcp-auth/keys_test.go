package main

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

func setupKeys(t *testing.T) {
	t.Helper()
	oauthEnabled = false
	passthroughUnknownBearer = false
	keysAdminSubjects = nil
	actorProxyBearerToken = "proxy-bearer"
	store, err := openKeysStore(filepath.Join(t.TempDir(), "keys.db"))
	if err != nil {
		t.Fatalf("openKeysStore: %v", err)
	}
	consumerKeys = store
	t.Cleanup(func() {
		_ = store.close()
		consumerKeys = nil
		actorProxyBearerToken = ""
		passthroughUnknownBearer = false
		keysAdminSubjects = nil
		jwtKeyfunc = nil
		oidcIssuer = ""
		hookAuthToken = ""
		hookAuthUsername = ""
		upstreamAuthToken = ""
	})
}

func keysReq(method, path, bearer, body string) *httptest.ResponseRecorder {
	var rdr *bytes.Reader
	if body != "" {
		rdr = bytes.NewReader([]byte(body))
	} else {
		rdr = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, rdr)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	rec := httptest.NewRecorder()
	handleKeys(rec, req)
	return rec
}

func mustInsertKey(t *testing.T, rec consumerKeyRecord) string {
	t.Helper()
	secret, err := generateSecret()
	if err != nil {
		t.Fatal(err)
	}
	last4, preview := previewOf(secret)
	rec.KeySHA256 = hashSecret(secret)
	rec.KeyLast4 = last4
	rec.Preview = preview
	if rec.CreatedAt == 0 {
		rec.CreatedAt = time.Now().Unix()
	}
	if rec.Owner.Label == "" {
		rec.Owner.Label = "tester"
	}
	if rec.Owner.Kind == "" {
		rec.Owner.Kind = "user"
	}
	if err := consumerKeys.insert(rec); err != nil {
		t.Fatalf("insert: %v", err)
	}
	return secret
}

func setupRSA(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	oidcIssuer = "https://idp.example/realms/test"
	jwtKeyfunc = func(token *jwt.Token) (any, error) {
		return &key.PublicKey, nil
	}
	return key
}

func signJWT(t *testing.T, key *rsa.PrivateKey, claims jwt.MapClaims) string {
	t.Helper()
	if _, ok := claims["iss"]; !ok {
		claims["iss"] = oidcIssuer
	}
	if _, ok := claims["exp"]; !ok {
		claims["exp"] = time.Now().Add(time.Hour).Unix()
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	s, err := tok.SignedString(key)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func adminJWT(t *testing.T, key *rsa.PrivateKey) string {
	t.Helper()
	return signJWT(t, key, jwt.MapClaims{
		"sub":                "user-1",
		"preferred_username": "alice",
		"realm_access":       map[string]any{"roles": []any{"mcp:admin"}},
	})
}

func TestKeysDisabledReturns404(t *testing.T) {
	consumerKeys = nil
	rec := keysReq(http.MethodGet, "/keys", "", "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 when KEYS_DB is unset", rec.Code)
	}
}

func TestIssueFailClosed(t *testing.T) {
	setupKeys(t)
	setupHookToken(t)

	cases := []struct {
		name, bearer string
	}{
		{"anonymous", ""},
		{"hook_token", "hook-secret"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := keysReq(http.MethodPost, "/keys", tc.bearer, `{"id":"cli","actor_user":"cli","scopes":["read"]}`)
			if rec.Code != http.StatusForbidden {
				t.Fatalf("status = %d, want 403", rec.Code)
			}
			if !strings.Contains(rec.Body.String(), "issuance requires an identified operator") {
				t.Errorf("body = %s, want fail-closed error", rec.Body.String())
			}
		})
	}
}

func TestOwnerNeverFromBody(t *testing.T) {
	setupKeys(t)
	rsaKey := setupRSA(t)
	bearer := adminJWT(t, rsaKey)

	body := `{
		"id":"claude-code",
		"actor_user":"claude-code",
		"scopes":["read","write"],
		"owner":{"kind":"subject","label":"evil","issuer":"https://evil","subject":"x"}
	}`
	rec := keysReq(http.MethodPost, "/keys", bearer, body)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 — owner must not be accepted from the body", rec.Code)
	}

	rec = keysReq(http.MethodPost, "/keys", bearer, `{
		"id":"claude-code",
		"actor_user":"claude-code",
		"scopes":["read","write"]
	}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", rec.Code, rec.Body.String())
	}
	var created consumerKeyJSON
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if created.Owner.Kind != "subject" || created.Owner.Label != "alice" || created.Owner.Subject != "user-1" {
		t.Errorf("owner = %+v, want JWT subject alice/user-1", created.Owner)
	}
	if !strings.HasPrefix(created.Key, "amk_") || len(created.Key) != 44 {
		t.Errorf("key = %q, want amk_ + 40 hex", created.Key)
	}
	if created.Preview != consumerKeyPreview+created.Key[len(created.Key)-4:] {
		t.Errorf("preview = %q", created.Preview)
	}
}

func TestCreateDuplicateID(t *testing.T) {
	setupKeys(t)
	rsaKey := setupRSA(t)
	bearer := adminJWT(t, rsaKey)
	body := `{"id":"dup","actor_user":"dup","scopes":["read"]}`
	if rec := keysReq(http.MethodPost, "/keys", bearer, body); rec.Code != http.StatusCreated {
		t.Fatalf("first create status = %d, body=%s", rec.Code, rec.Body.String())
	}
	rec := keysReq(http.MethodPost, "/keys", bearer, body)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409", rec.Code)
	}
}

func TestCreateInvalidScope(t *testing.T) {
	setupKeys(t)
	rsaKey := setupRSA(t)
	bearer := adminJWT(t, rsaKey)
	rec := keysReq(http.MethodPost, "/keys", bearer, `{"id":"x","actor_user":"x","scopes":["read","super"]}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestCreateInvalidID(t *testing.T) {
	setupKeys(t)
	rsaKey := setupRSA(t)
	bearer := adminJWT(t, rsaKey)
	for _, id := range []string{"A", "-ab", "a", "HasCaps"} {
		rec := keysReq(http.MethodPost, "/keys", bearer, `{"id":"`+id+`","actor_user":"x","scopes":["read"]}`)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("id %q status = %d, want 400", id, rec.Code)
		}
	}
}

func TestScopeRouteGate(t *testing.T) {
	setupKeys(t)
	readKey := mustInsertKey(t, consumerKeyRecord{ID: "r", ActorUser: "reader", Scopes: []string{"read"}})
	writeKey := mustInsertKey(t, consumerKeyRecord{ID: "w", ActorUser: "writer", Scopes: []string{"write"}})
	adminKey := mustInsertKey(t, consumerKeyRecord{
		ID: "a", ActorUser: "admin", Scopes: []string{"admin"},
		Owner: keyOwner{Kind: "subject", Label: "alice", Issuer: oidcIssuer, Subject: "user-1"},
	})

	type row struct {
		name, key, method, uri string
		want                   int
	}
	cases := []row{
		{"read GET mcp", readKey, "GET", "/wiki/mcp", 200},
		{"read GET api", readKey, "GET", "/api/v1/pages", 200},
		{"read GET web", readKey, "GET", "/web/", 200},
		// JSON-RPC multiplexes read tools (memory_query) and write tools
		// (memory_write_page, memory_forget, …) on the same POST /mcp path.
		// forwardAuth only sees method+path, so a read key must not pass.
		{"read POST mcp", readKey, "POST", "/wiki/mcp", 403},
		{"read POST mcp unprefixed", readKey, "POST", "/mcp", 403},
		{"read POST mcp/admin", readKey, "POST", "/wiki/mcp/admin/x", 403},
		{"read DELETE mcp", readKey, "DELETE", "/wiki/mcp", 403},
		{"read POST hook", readKey, "POST", "/wiki/hook", 403},
		{"read GET handoff", readKey, "GET", "/handoff", 403},
		{"read GET admin", readKey, "GET", "/admin/status", 403},
		{"write GET mcp", writeKey, "GET", "/wiki/mcp", 200},
		{"write POST mcp", writeKey, "POST", "/wiki/mcp", 200},
		{"write POST mcp unprefixed", writeKey, "POST", "/mcp", 200},
		{"write POST mcp/write", writeKey, "POST", "/wiki/mcp/write/x", 200},
		{"write POST mcp/admin", writeKey, "POST", "/wiki/mcp/admin/x", 200},
		{"write DELETE mcp", writeKey, "DELETE", "/wiki/mcp", 200},
		{"write POST hook", writeKey, "POST", "/wiki/hook", 200},
		{"write GET handoff", writeKey, "GET", "/handoff", 200},
		{"write GET admin", writeKey, "GET", "/admin/status", 403},
		{"write POST admin", writeKey, "POST", "/admin/backup", 403},
		{"admin GET admin", adminKey, "GET", "/admin/status", 200},
		{"admin POST admin", adminKey, "POST", "/admin/backup", 200},
		{"admin POST mcp", adminKey, "POST", "/wiki/mcp", 200},
		{"admin POST hook", adminKey, "POST", "/wiki/hook", 200},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := verifyWith(tc.method, tc.uri, tc.key)
			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d", rec.Code, tc.want)
			}
		})
	}
}

func TestRevokedAndExpiredVerify401(t *testing.T) {
	setupKeys(t)
	rsaKey := setupRSA(t)
	admin := adminJWT(t, rsaKey)

	secret := mustInsertKey(t, consumerKeyRecord{ID: "live", ActorUser: "cli", Scopes: []string{"read"}})
	past := time.Now().Unix() - 60
	expiredSecret := mustInsertKey(t, consumerKeyRecord{
		ID: "old", ActorUser: "cli", Scopes: []string{"read"}, ExpiresAt: &past,
	})

	if rec := verifyWith("GET", "/wiki/mcp", secret); rec.Code != http.StatusOK {
		t.Fatalf("live key status = %d, want 200", rec.Code)
	}

	del := keysReq(http.MethodDelete, "/keys/live", admin, "")
	if del.Code != http.StatusNoContent {
		t.Fatalf("revoke status = %d, want 204; body=%s", del.Code, del.Body.String())
	}
	// Idempotent.
	if rec := keysReq(http.MethodDelete, "/keys/live", admin, ""); rec.Code != http.StatusNoContent {
		t.Fatalf("second revoke status = %d, want 204", rec.Code)
	}
	if rec := verifyWith("GET", "/wiki/mcp", secret); rec.Code != http.StatusUnauthorized {
		t.Fatalf("revoked key status = %d, want 401", rec.Code)
	}
	if rec := verifyWith("GET", "/wiki/mcp", expiredSecret); rec.Code != http.StatusUnauthorized {
		t.Fatalf("expired key status = %d, want 401", rec.Code)
	}
	if rec := verifyWith("GET", "/wiki/mcp", "amk_"+strings.Repeat("ab", 20)); rec.Code != http.StatusUnauthorized {
		t.Fatalf("unknown key status = %d, want 401", rec.Code)
	}
}

func TestLastUsedAtUpdated(t *testing.T) {
	setupKeys(t)
	secret := mustInsertKey(t, consumerKeyRecord{ID: "touch", ActorUser: "cli", Scopes: []string{"read"}})
	if rec := verifyWith("GET", "/wiki/mcp", secret); rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		got, err := consumerKeys.getByID("touch")
		if err != nil {
			t.Fatal(err)
		}
		if got.LastUsedAt != nil && *got.LastUsedAt > 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("last_used_at was not updated")
}

func TestPassthroughUnknownBearer(t *testing.T) {
	setupKeys(t)

	passthroughUnknownBearer = false
	if rec := verifyWith("GET", "/wiki/mcp", "cli-static-token"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("passthrough off status = %d, want 401", rec.Code)
	}

	passthroughUnknownBearer = true
	rec := verifyWith("GET", "/wiki/mcp", "cli-static-token")
	if rec.Code != http.StatusOK {
		t.Fatalf("passthrough on status = %d, want 200", rec.Code)
	}
	if got := rec.Header().Get("Authorization"); got != "" {
		t.Errorf("Authorization = %q, want untouched (empty on the auth response)", got)
	}

	// amk_ still 401s even with passthrough — it is a consumer key, just unknown.
	if rec := verifyWith("GET", "/wiki/mcp", "amk_"+strings.Repeat("cd", 20)); rec.Code != http.StatusUnauthorized {
		t.Fatalf("unknown amk_ with passthrough status = %d, want 401", rec.Code)
	}
}

func TestWhoami(t *testing.T) {
	setupKeys(t)
	rsaKey := setupRSA(t)

	rec := keysReq(http.MethodGet, "/keys/whoami", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("anonymous whoami status = %d, want 200", rec.Code)
	}
	var anon struct {
		Identity *keyOwner `json:"identity"`
		CanIssue bool      `json:"can_issue"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &anon); err != nil {
		t.Fatal(err)
	}
	if anon.Identity != nil || anon.CanIssue {
		t.Errorf("anonymous whoami = %+v, want identity=null can_issue=false", anon)
	}

	userTok := signJWT(t, rsaKey, jwt.MapClaims{
		"sub":                "user-2",
		"preferred_username": "bob",
		"realm_access":       map[string]any{"roles": []any{"mcp:read"}},
	})
	rec = keysReq(http.MethodGet, "/keys/whoami", userTok, "")
	var user struct {
		Identity *keyOwner `json:"identity"`
		CanIssue bool      `json:"can_issue"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &user); err != nil {
		t.Fatal(err)
	}
	if user.Identity == nil || user.Identity.Label != "bob" || user.CanIssue {
		t.Errorf("user whoami = %+v, want bob without issue privilege", user)
	}

	adminTok := adminJWT(t, rsaKey)
	rec = keysReq(http.MethodGet, "/keys/whoami", adminTok, "")
	var admin struct {
		Identity *keyOwner `json:"identity"`
		CanIssue bool      `json:"can_issue"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &admin); err != nil {
		t.Fatal(err)
	}
	if admin.Identity == nil || admin.Identity.Label != "alice" || !admin.CanIssue {
		t.Errorf("admin whoami = %+v, want alice can_issue=true", admin)
	}
}

func TestActorHeadersReplaceAndNoPartialPair(t *testing.T) {
	setupKeys(t)
	writeSecret := mustInsertKey(t, consumerKeyRecord{ID: "w", ActorUser: "writer", Scopes: []string{"write"}})
	adminSecret := mustInsertKey(t, consumerKeyRecord{
		ID: "a", ActorUser: "rootish", Scopes: []string{"admin"},
		Owner: keyOwner{Kind: "subject", Label: "alice", Issuer: "https://idp.example/realms/test", Subject: "user-1"},
	})
	userOwnedAdmin := mustInsertKey(t, consumerKeyRecord{
		ID: "u", ActorUser: "bot", Scopes: []string{"admin"},
		Owner: keyOwner{Kind: "user", Label: "bot"},
	})

	verifyForged := func(method, uri, bearer string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/verify", nil)
		req.Header.Set("X-Forwarded-Method", method)
		req.Header.Set("X-Forwarded-Uri", uri)
		req.Header.Set("Authorization", "Bearer "+bearer)
		req.Header.Set("X-Memory-Actor-User", "forged-user")
		req.Header.Set("X-Memory-Actor-Issuer", "forged-iss")
		req.Header.Set("X-Memory-Actor-Sub", "forged-sub")
		rec := httptest.NewRecorder()
		handleVerify(rec, req)
		return rec
	}

	rec := verifyForged("GET", "/wiki/mcp", writeSecret)
	if rec.Code != http.StatusOK {
		t.Fatalf("write key status = %d", rec.Code)
	}
	if got := rec.Header().Get("Authorization"); got != "Bearer proxy-bearer" {
		t.Errorf("Authorization = %q, want injected proxy bearer", got)
	}
	if got := rec.Header().Get("X-Memory-Actor-User"); got != "writer" {
		t.Errorf("X-Memory-Actor-User = %q, want writer (replaced)", got)
	}
	if rec.Header().Get("X-Memory-Actor-Issuer") != "" || rec.Header().Get("X-Memory-Actor-Sub") != "" {
		t.Errorf("write key emitted OIDC pair issuer=%q sub=%q", rec.Header().Get("X-Memory-Actor-Issuer"), rec.Header().Get("X-Memory-Actor-Sub"))
	}
	if len(rec.Header().Values("X-Memory-Actor-User")) != 1 {
		t.Errorf("X-Memory-Actor-User values = %v, want a single replaced value", rec.Header().Values("X-Memory-Actor-User"))
	}

	rec = verifyForged("GET", "/admin/status", adminSecret)
	if rec.Code != http.StatusOK {
		t.Fatalf("admin subject key status = %d", rec.Code)
	}
	if rec.Header().Get("X-Memory-Actor-Issuer") != "https://idp.example/realms/test" || rec.Header().Get("X-Memory-Actor-Sub") != "user-1" {
		t.Errorf("admin subject pair issuer=%q sub=%q", rec.Header().Get("X-Memory-Actor-Issuer"), rec.Header().Get("X-Memory-Actor-Sub"))
	}

	rec = verifyForged("GET", "/admin/status", userOwnedAdmin)
	if rec.Code != http.StatusOK {
		t.Fatalf("admin user-kind key status = %d", rec.Code)
	}
	if rec.Header().Get("X-Memory-Actor-Issuer") != "" || rec.Header().Get("X-Memory-Actor-Sub") != "" {
		t.Errorf("user-kind admin emitted partial pair issuer=%q sub=%q", rec.Header().Get("X-Memory-Actor-Issuer"), rec.Header().Get("X-Memory-Actor-Sub"))
	}
}

func TestKeysAdminSubjectsFallback(t *testing.T) {
	setupKeys(t)
	rsaKey := setupRSA(t)
	keysAdminSubjects = []adminSubjectPair{{issuer: oidcIssuer, subject: "allow-me"}}
	tok := signJWT(t, rsaKey, jwt.MapClaims{
		"sub":                "allow-me",
		"preferred_username": "carol",
		"realm_access":       map[string]any{"roles": []any{"mcp:read"}},
	})
	rec := keysReq(http.MethodPost, "/keys", tok, `{"id":"from-env","actor_user":"from-env","scopes":["read"]}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 via KEYS_ADMIN_SUBJECTS; body=%s", rec.Code, rec.Body.String())
	}
}

func TestListKeysShape(t *testing.T) {
	setupKeys(t)
	rsaKey := setupRSA(t)
	admin := adminJWT(t, rsaKey)
	mustInsertKey(t, consumerKeyRecord{ID: "one", ActorUser: "one", Scopes: []string{"read"}})
	rec := keysReq(http.MethodGet, "/keys", admin, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var payload struct {
		Keys []consumerKeyJSON `json:"keys"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.Keys) != 1 || payload.Keys[0].ID != "one" || payload.Keys[0].Key != "" {
		t.Errorf("list = %+v, want one key without plaintext", payload.Keys)
	}
}
