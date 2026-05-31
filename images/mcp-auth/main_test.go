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
