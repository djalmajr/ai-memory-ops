package main

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/golang-jwt/jwt/v5"
)

func TestMain(m *testing.M) {
	// main() normally initializes the logger; in tests we point it to io.Discard.
	logger = slog.New(slog.NewJSONHandler(io.Discard, nil))
	os.Exit(m.Run())
}

// Tests the RFC 9728 metadata + the WWW-Authenticate challenge.
// Does not require Keycloak: the metadata handlers and the "missing bearer" path of
// /verify do not touch the JWKS.

func setupOAuth() {
	oauthEnabled = true
	oidcIssuer = "https://keycloak.example.com/realms/ai-memory-svc"
	oauthResource = "https://platform.example.com/wiki/mcp"
	oauthMetadataURL = "https://platform.example.com/wiki/.well-known/oauth-protected-resource"
	oauthScopes = []string{"mcp:read", "mcp:write"}
}

func TestProtectedResourceMetadata(t *testing.T) {
	setupOAuth()
	req := httptest.NewRequest(http.MethodGet, "/.well-known/oauth-protected-resource", nil)
	rec := httptest.NewRecorder()
	handleProtectedResourceMetadata(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var meta struct {
		Resource               string   `json:"resource"`
		AuthorizationServers   []string `json:"authorization_servers"`
		BearerMethodsSupported []string `json:"bearer_methods_supported"`
		ScopesSupported        []string `json:"scopes_supported"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &meta); err != nil {
		t.Fatalf("invalid json: %v — body=%s", err, rec.Body.String())
	}
	if meta.Resource != oauthResource {
		t.Errorf("resource = %q, want %q", meta.Resource, oauthResource)
	}
	if len(meta.AuthorizationServers) != 1 || meta.AuthorizationServers[0] != oidcIssuer {
		t.Errorf("authorization_servers = %v, want [%q]", meta.AuthorizationServers, oidcIssuer)
	}
	if len(meta.ScopesSupported) != 2 {
		t.Errorf("scopes_supported = %v, want 2 items", meta.ScopesSupported)
	}
}

func TestProtectedResourceMetadataDisabled(t *testing.T) {
	oauthEnabled = false
	req := httptest.NewRequest(http.MethodGet, "/.well-known/oauth-protected-resource", nil)
	rec := httptest.NewRecorder()
	handleProtectedResourceMetadata(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 when oauthEnabled=false", rec.Code)
	}
}

func TestVerifyMissingBearerEmitsChallenge(t *testing.T) {
	setupOAuth()
	req := httptest.NewRequest(http.MethodPost, "/verify", nil)
	req.Header.Set("X-Forwarded-Method", "POST")
	req.Header.Set("X-Forwarded-Uri", "/wiki/mcp")
	rec := httptest.NewRecorder()
	handleVerify(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	wa := rec.Header().Get("WWW-Authenticate")
	if !strings.HasPrefix(wa, "Bearer ") || !strings.Contains(wa, "resource_metadata=") {
		t.Errorf("WWW-Authenticate = %q, want Bearer with resource_metadata", wa)
	}
	if !strings.Contains(wa, oauthMetadataURL) {
		t.Errorf("WWW-Authenticate does not contain the metadata URL %q: %q", oauthMetadataURL, wa)
	}
}

func TestVerifyChallengeNoopWhenDisabled(t *testing.T) {
	oauthEnabled = false
	req := httptest.NewRequest(http.MethodPost, "/verify", nil)
	req.Header.Set("X-Forwarded-Method", "POST")
	req.Header.Set("X-Forwarded-Uri", "/wiki/mcp")
	rec := httptest.NewRecorder()
	handleVerify(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	if wa := rec.Header().Get("WWW-Authenticate"); wa != "" {
		t.Errorf("WWW-Authenticate = %q, want empty when oauthEnabled=false", wa)
	}
}

// nginx-ingress auth_request sends X-Original-Method + an absolute X-Original-URL
// instead of the Traefik X-Forwarded-* pair. forwardedRoute must normalize both.
func TestForwardedRouteNginxHeaders(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/verify", nil)
	req.Header.Set("X-Original-Method", "POST")
	req.Header.Set("X-Original-URL", "https://memory.example.com/wiki/mcp/write/foo?x=1")
	method, uri := forwardedRoute(req)
	if method != "POST" {
		t.Errorf("method = %q, want POST", method)
	}
	if uri != "/wiki/mcp/write/foo?x=1" {
		t.Errorf("uri = %q, want /wiki/mcp/write/foo?x=1", uri)
	}
}

// Traefik headers take precedence and are returned as-is.
func TestForwardedRouteTraefikHeaders(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/verify", nil)
	req.Header.Set("X-Forwarded-Method", "GET")
	req.Header.Set("X-Forwarded-Uri", "/wiki/mcp")
	method, uri := forwardedRoute(req)
	if method != "GET" || uri != "/wiki/mcp" {
		t.Errorf("got (%q,%q), want (GET,/wiki/mcp)", method, uri)
	}
}

// The missing-bearer challenge path must also work when only nginx headers are present.
func TestVerifyMissingBearerNginxHeaders(t *testing.T) {
	setupOAuth()
	req := httptest.NewRequest(http.MethodPost, "/verify", nil)
	req.Header.Set("X-Original-Method", "POST")
	req.Header.Set("X-Original-URL", "https://memory.example.com/wiki/mcp")
	rec := httptest.NewRecorder()
	handleVerify(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	if wa := rec.Header().Get("WWW-Authenticate"); !strings.Contains(wa, "resource_metadata=") {
		t.Errorf("WWW-Authenticate = %q, want resource_metadata challenge", wa)
	}
}

func TestWellKnownIsPublicPath(t *testing.T) {
	cases := map[string]bool{
		"/wiki/.well-known/oauth-protected-resource": true,
		"/.well-known/oauth-protected-resource":      true,
		"/wiki/mcp":                                  false,
		"/wiki/healthz":                              true,
	}
	for uri, want := range cases {
		if got := isPublicPath(uri); got != want {
			t.Errorf("isPublicPath(%q) = %v, want %v", uri, got, want)
		}
	}
}

// Lifecycle-hook token tests. None of them require Keycloak: the hook-token
// branch returns before the JWKS is consulted, and the fall-through paths hit
// the jwks==nil guard (401) under test.

func setupHookToken(t *testing.T) {
	t.Helper()
	oauthEnabled = false
	hookAuthToken = "hook-secret"
	hookAuthUsername = "djalmajr"
	upstreamAuthToken = "upstream-static"
	t.Cleanup(func() {
		hookAuthToken = ""
		hookAuthUsername = ""
		upstreamAuthToken = ""
	})
}

func verifyWith(method, uri, bearer string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/verify", nil)
	req.Header.Set("X-Forwarded-Method", method)
	req.Header.Set("X-Forwarded-Uri", uri)
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	rec := httptest.NewRecorder()
	handleVerify(rec, req)
	return rec
}

func TestVerifyHookTokenOnHookPath(t *testing.T) {
	setupHookToken(t)
	rec := verifyWith("POST", "/wiki/hook?event=pre-tool-use&agent=claude-code", "hook-secret")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := rec.Header().Get("X-Memory-Actor-User"); got != "djalmajr" {
		t.Errorf("X-Memory-Actor-User = %q, want djalmajr", got)
	}
	if got := rec.Header().Get("X-Auth-Username"); got != "djalmajr" {
		t.Errorf("X-Auth-Username = %q, want djalmajr", got)
	}
	if got := rec.Header().Get("Authorization"); got != "Bearer upstream-static" {
		t.Errorf("Authorization = %q, want the injected upstream bearer", got)
	}
}

// The engine (>= v1.22.0) treats issuer+subject as a PAIR — a partial pair is
// rejected with 400 — so whenever the subject is asserted the issuer must ride
// along. A token without `iss` never validates, so the pair is always complete.
func TestPropagateIdentityHeadersAssertsIssuerBesideSubject(t *testing.T) {
	rec := httptest.NewRecorder()
	claims := jwt.MapClaims{
		"preferred_username": "djalmajr",
		"sub":                "user-sub",
		"iss":                "https://idp.example/realms/ai-memory",
	}
	propagateIdentityHeaders(rec, claims)
	if got := rec.Header().Get("X-Memory-Actor-Sub"); got != "user-sub" {
		t.Errorf("X-Memory-Actor-Sub = %q, want user-sub", got)
	}
	if got := rec.Header().Get("X-Memory-Actor-Issuer"); got != "https://idp.example/realms/ai-memory" {
		t.Errorf("X-Memory-Actor-Issuer = %q, want the token issuer", got)
	}
}

func TestPropagateIdentityHeadersDoesNotUseJwtSidAsActorSession(t *testing.T) {
	rec := httptest.NewRecorder()
	claims := jwt.MapClaims{
		"email":              "dev@example.com",
		"preferred_username": "djalmajr",
		"sub":                "user-sub",
		"azp":                "client-id",
		"client_name":        "Codex",
		"sid":                "keycloak-login-session",
	}

	propagateIdentityHeaders(rec, claims)

	headers := rec.Header()
	if got := headers.Get("X-Auth-Email"); got != "dev@example.com" {
		t.Errorf("X-Auth-Email = %q, want dev@example.com", got)
	}
	if got := headers.Get("X-Auth-Username"); got != "djalmajr" {
		t.Errorf("X-Auth-Username = %q, want djalmajr", got)
	}
	if got := headers.Get("X-Auth-Sub"); got != "user-sub" {
		t.Errorf("X-Auth-Sub = %q, want user-sub", got)
	}
	if got := headers.Get("X-Memory-Actor-User"); got != "djalmajr" {
		t.Errorf("X-Memory-Actor-User = %q, want djalmajr", got)
	}
	if got := headers.Get("X-Memory-Actor-Sub"); got != "user-sub" {
		t.Errorf("X-Memory-Actor-Sub = %q, want user-sub", got)
	}
	if got := headers.Get("X-Memory-Actor-Client"); got != "client-id" {
		t.Errorf("X-Memory-Actor-Client = %q, want client-id", got)
	}
	if got := headers.Get("X-Memory-Actor-Agent"); got != "Codex" {
		t.Errorf("X-Memory-Actor-Agent = %q, want Codex", got)
	}
	if got := headers.Get("X-Memory-Actor-Session-Id"); got != "" {
		t.Errorf("X-Memory-Actor-Session-Id = %q, want empty for Keycloak sid", got)
	}
}

func TestVerifyHookTokenOnHandoffPath(t *testing.T) {
	setupHookToken(t)
	rec := verifyWith("GET", "/handoff?agent=claude-code", "hook-secret")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
}

func TestVerifyHookTokenRejectedOutsideHookPaths(t *testing.T) {
	setupHookToken(t)
	rec := verifyWith("POST", "/wiki/mcp", "hook-secret")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 — hook token must not work outside /hook|/handoff", rec.Code)
	}
}

func TestVerifyHookTokenWrongTokenFallsThrough(t *testing.T) {
	setupHookToken(t)
	rec := verifyWith("POST", "/wiki/hook?event=stop&agent=claude-code", "not-the-secret")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 for a wrong hook token", rec.Code)
	}
	if got := rec.Header().Get("Authorization"); got != "" {
		t.Errorf("Authorization = %q, want empty (no upstream injection on 401)", got)
	}
}

func TestVerifyHookTokenDisabledKeepsCurrentBehavior(t *testing.T) {
	oauthEnabled = false
	hookAuthToken = ""
	rec := verifyWith("POST", "/wiki/hook?event=stop&agent=claude-code", "hook-secret")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 when HOOK_AUTH_TOKEN is unset", rec.Code)
	}
}

func TestIsHookPath(t *testing.T) {
	cases := map[string]bool{
		"/hook":                          true,
		"/handoff":                       true,
		"/wiki/hook?event=stop&agent=cc": true,
		"/wiki/handoff?agent=cc":         true,
		"/wiki/mcp":                      false,
		"/mcp/hook":                      false,
		"/wiki/hooks":                    false,
		"/wiki/pages/hook":               false,
	}
	for uri, want := range cases {
		if got := isHookPath(uri); got != want {
			t.Errorf("isHookPath(%q) = %v, want %v", uri, got, want)
		}
	}
}

// Every 200 out of /verify must name the Authorization the upstream should see.
// A `copy_headers`-style integration (Caddy forward_auth, Traefik
// authResponseHeaders) DELETES a listed header the auth response omits — so a
// silent 200 destroys the caller's bearer and the upstream 401s. Verified
// against Caddy 2: a 200 without Authorization made the upstream see none.
func TestVerifyAlwaysNamesUpstreamAuthorization(t *testing.T) {
	t.Run("passthrough echoes the unknown bearer", func(t *testing.T) {
		oauthEnabled = false
		hookAuthToken = ""
		passthroughUnknownBearer = true
		// Configured on purpose: passthrough must NOT reach for it.
		upstreamAuthToken = "upstream-static"
		t.Cleanup(func() {
			passthroughUnknownBearer = false
			upstreamAuthToken = ""
		})

		rec := verifyWith("POST", "/wiki/mcp", "cli-token-nao-migrado")
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rec.Code)
		}
		if got := rec.Header().Get("Authorization"); got != "Bearer cli-token-nao-migrado" {
			t.Errorf("Authorization = %q, want the caller's own bearer echoed", got)
		}
	})

	// Swapping an UNKNOWN bearer for the upstream token would be an auth
	// bypass: garbage in, valid credential out.
	t.Run("passthrough never injects the upstream token", func(t *testing.T) {
		oauthEnabled = false
		hookAuthToken = ""
		passthroughUnknownBearer = true
		upstreamAuthToken = "upstream-static"
		t.Cleanup(func() {
			passthroughUnknownBearer = false
			upstreamAuthToken = ""
		})

		rec := verifyWith("POST", "/wiki/mcp", "lixo")
		if got := rec.Header().Get("Authorization"); got == "Bearer upstream-static" {
			t.Fatal("passthrough injected the upstream token: unknown bearer upgraded to a valid one")
		}
	})

	t.Run("hook token echoes when no upstream token is configured", func(t *testing.T) {
		oauthEnabled = false
		hookAuthToken = "hook-secret"
		hookAuthUsername = "djalmajr"
		upstreamAuthToken = ""
		t.Cleanup(func() {
			hookAuthToken = ""
			hookAuthUsername = ""
		})

		rec := verifyWith("POST", "/wiki/hook?event=stop&agent=cc", "hook-secret")
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rec.Code)
		}
		if got := rec.Header().Get("Authorization"); got != "Bearer hook-secret" {
			t.Errorf("Authorization = %q, want the hook bearer echoed", got)
		}
	})

	t.Run("public path echoes and never injects", func(t *testing.T) {
		oauthEnabled = false
		hookAuthToken = ""
		upstreamAuthToken = "upstream-static"
		t.Cleanup(func() { upstreamAuthToken = "" })

		rec := verifyWith("GET", "/wiki/healthz", "qualquer-coisa")
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rec.Code)
		}
		if got := rec.Header().Get("Authorization"); got != "Bearer qualquer-coisa" {
			t.Errorf("Authorization = %q, want the caller's header echoed, not the upstream token", got)
		}
	})

	// No Authorization in, none out — echoing an empty value would forge a
	// header the caller never sent.
	t.Run("public path with no bearer sets nothing", func(t *testing.T) {
		oauthEnabled = false
		hookAuthToken = ""
		upstreamAuthToken = "upstream-static"
		t.Cleanup(func() { upstreamAuthToken = "" })

		rec := verifyWith("GET", "/wiki/healthz", "")
		if got := rec.Header().Get("Authorization"); got != "" {
			t.Errorf("Authorization = %q, want empty", got)
		}
	})
}
