// mcp-auth — JWT validator sidecar for Traefik forwardAuth.
//
// Receives forwardAuth from Traefik via the `X-Forwarded-Uri` header. Validates the JWT in the
// `Authorization: Bearer <token>` header against the public Keycloak JWKS
// (issuer = OIDC_ISSUER env), checks expiration, optional audience, and requires a
// realm-level role based on the original route (X-Forwarded-Uri):
//
//   - GET   /mcp/...  → role `mcp:read`
//   - POST  /mcp/...  → role `mcp:read` (queries are POST JSON-RPC)
//   - write routes → role `mcp:write`
//
// Endpoints:
//
//	GET /healthz   → 200 always (k8s probe)
//	GET /readyz    → 200 if JWKS was fetched successfully at least once
//	GET /verify    → 200 (authorized) / 401 (missing/invalid token) / 403 (valid token without role)
//
// Env vars:
//
//	OIDC_ISSUER         (required) — https://keycloak.example.com/realms/ai-memory-svc
//	OIDC_AUDIENCE       (optional) — expects this aud in the JWT (default: empty = not checked)
//	PORT                (default 8081)
//	LOG_LEVEL           (default info; debug|info|warn|error)
//	JWKS_REFRESH_SECONDS (default 300 = 5min)
//
// Logs: JSON-line via log/slog; never logs the token, only claims without PII (sub, exp, scope).
package main

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/MicahParks/keyfunc/v3"
	"github.com/golang-jwt/jwt/v5"
)

var (
	logger     *slog.Logger
	jwks       keyfunc.Keyfunc
	jwksReady  bool
	oidcIssuer        string
	oidcAud           string
	upstreamAuthToken string // optional: if set, injects Authorization: Bearer <token> into the upstream after validating the JWT (ai-memory uses a static AI_MEMORY_AUTH_TOKEN and does not validate JWT)
	httpClient        = &http.Client{Timeout: 10 * time.Second}

	// OAuth protected resource. When oauthEnabled, the
	// validator serves RFC 9728 metadata and emits WWW-Authenticate on 401s, so
	// that MCP clients (Claude Code/Desktop) discover Keycloak and perform
	// Authorization Code + PKCE with transparent refresh (no manual token).
	oauthEnabled     bool
	oauthResource    string // identifier of the protected resource (e.g.: https://platform.example.com/wiki/mcp)
	oauthMetadataURL string // public URL of the metadata (e.g.: https://platform.example.com/wiki/.well-known/oauth-protected-resource)
	oauthScopes      []string
)

func main() {
	port := envOr("PORT", "8081")
	oidcIssuer = strings.TrimRight(envRequired("OIDC_ISSUER"), "/")
	oidcAud = os.Getenv("OIDC_AUDIENCE") // optional
	upstreamAuthToken = os.Getenv("UPSTREAM_AUTH_TOKEN") // optional: static token injected into the upstream
	refresh := envIntOr("JWKS_REFRESH_SECONDS", 300)

	// OAuth protected resource. Opt-in via OAUTH_ENABLED=true.
	oauthEnabled = strings.EqualFold(os.Getenv("OAUTH_ENABLED"), "true")
	oauthResource = strings.TrimRight(os.Getenv("OAUTH_RESOURCE"), "/")
	oauthMetadataURL = strings.TrimRight(os.Getenv("OAUTH_RESOURCE_METADATA_URL"), "/")
	// `profile` and `offline_access` are part of the default set so:
	// - `profile` makes the JWT carry `preferred_username` → mcp-auth propagates
	//   it as `X-Memory-Actor-User` → ai-memory's contributors-webhook can
	//   attribute writes to a human-friendly username (not just the UUID sub).
	// - `offline_access` lets DCR clients (Codex/Claude Code MCP) keep a
	//   refresh token so they survive token expiry without re-prompting.
	oauthScopes = strings.Fields(envOr("OAUTH_SCOPES", "mcp:read mcp:write offline_access profile"))

	logLevel := parseLogLevel(envOr("LOG_LEVEL", "info"))
	logger = slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: logLevel}))
	slog.SetDefault(logger)

	logger.Info("startup",
		"port", port,
		"oidc_issuer", oidcIssuer,
		"oidc_audience", oidcAud,
		"upstream_token_inject", upstreamAuthToken != "",
		"jwks_refresh_seconds", refresh,
		"oauth_enabled", oauthEnabled,
		"oauth_resource", oauthResource,
	)

	if err := initJWKS(refresh); err != nil {
		logger.Error("jwks_init_failed", "error", err)
		os.Exit(1)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", handleHealthz)
	mux.HandleFunc("/readyz", handleReadyz)
	mux.HandleFunc("/verify", handleVerify)
	// RFC 9728 — served directly (dedicated Traefik route, no forwardAuth).
	mux.HandleFunc("/.well-known/oauth-protected-resource", handleProtectedResourceMetadata)

	logger.Info("listening", "addr", ":"+port)
	if err := http.ListenAndServe(":"+port, mux); err != nil {
		logger.Error("listen_failed", "error", err)
		os.Exit(1)
	}
}

// initJWKS fetches JWKS once at startup and sets up auto-refresh.
// Issuer discovery: GET <issuer>/.well-known/openid-configuration → jwks_uri.
func initJWKS(refreshSeconds int) error {
	discoveryURL := oidcIssuer + "/.well-known/openid-configuration"
	logger.Info("oidc_discovery", "url", discoveryURL)

	jwksURL, err := fetchJWKSURI(discoveryURL)
	if err != nil {
		return err
	}
	logger.Info("jwks_uri_resolved", "jwks_uri", jwksURL)

	k, err := keyfunc.NewDefaultCtx(context.Background(), []string{jwksURL})
	if err != nil {
		return err
	}
	jwks = k
	jwksReady = true
	return nil
}

func fetchJWKSURI(discoveryURL string) (string, error) {
	resp, err := httpClient.Get(discoveryURL)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", errors.New("OIDC discovery returned status " + resp.Status)
	}
	var cfg struct {
		JWKSURI string `json:"jwks_uri"`
	}
	if err := decodeJSON(resp, &cfg); err != nil {
		return "", err
	}
	if cfg.JWKSURI == "" {
		return "", errors.New("OIDC discovery missing jwks_uri")
	}
	return cfg.JWKSURI, nil
}

func handleHealthz(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"status":"ok"}`))
}

func handleReadyz(w http.ResponseWriter, r *http.Request) {
	if !jwksReady {
		http.Error(w, `{"status":"not-ready","reason":"jwks-not-fetched"}`, http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"status":"ready"}`))
}

// handleProtectedResourceMetadata serves the OAuth 2.0 Protected Resource Metadata
// (RFC 9728). Public (no auth). Points to Keycloak (OIDC_ISSUER) as the
// authorization server, allowing the MCP client to discover and start
// Authorization Code + PKCE.
func handleProtectedResourceMetadata(w http.ResponseWriter, r *http.Request) {
	if !oauthEnabled {
		http.NotFound(w, r)
		return
	}
	resource := oauthResource
	if resource == "" {
		// Derives from the forwarded host when not configured explicitly.
		resource = forwardedBaseURL(r)
	}
	meta := map[string]any{
		"resource":                 resource,
		"authorization_servers":    []string{oidcIssuer},
		"bearer_methods_supported": []string{"header"},
		"scopes_supported":         oauthScopes,
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(meta)
}

// setChallenge adds the WWW-Authenticate header (RFC 9728 §5.1) on 401s, with
// resource_metadata pointing to the metadata URL — the MCP client uses this to
// discover the authorization server. No-op when oauthEnabled=false (preserves
// legacy behavior). Traefik forwardAuth propagates this header to the client.
func setChallenge(w http.ResponseWriter, r *http.Request) {
	if !oauthEnabled {
		return
	}
	mu := oauthMetadataURL
	if mu == "" {
		mu = forwardedBaseURL(r) + "/.well-known/oauth-protected-resource"
	}
	w.Header().Set("WWW-Authenticate", `Bearer resource_metadata="`+mu+`"`)
}

// forwardedRoute extracts the original request method and URI from the reverse
// proxy headers, supporting both ingress flavors:
//   - Traefik forwardAuth: X-Forwarded-Method + X-Forwarded-Uri (path+query)
//   - nginx-ingress auth_request: X-Original-Method + X-Original-URL (absolute URL)
//
// The nginx absolute URL is reduced to path+query so downstream path matching
// (isPublicPath / roleForRoute) behaves identically across proxies.
func forwardedRoute(r *http.Request) (method, uri string) {
	method = r.Header.Get("X-Forwarded-Method")
	if method == "" {
		method = r.Header.Get("X-Original-Method")
	}
	uri = r.Header.Get("X-Forwarded-Uri")
	if uri == "" {
		if orig := r.Header.Get("X-Original-URL"); orig != "" {
			if u, err := url.Parse(orig); err == nil && u.Path != "" {
				uri = u.RequestURI()
			} else {
				uri = orig
			}
		}
	}
	return method, uri
}

// forwardedBaseURL reconstructs scheme://host from the proxy headers. Prefers the
// Traefik X-Forwarded-* pair; falls back to the absolute nginx X-Original-URL.
func forwardedBaseURL(r *http.Request) string {
	host := r.Header.Get("X-Forwarded-Host")
	scheme := r.Header.Get("X-Forwarded-Proto")
	if host == "" || scheme == "" {
		if orig := r.Header.Get("X-Original-URL"); orig != "" {
			if u, err := url.Parse(orig); err == nil && u.Host != "" {
				if host == "" {
					host = u.Host
				}
				if scheme == "" {
					scheme = u.Scheme
				}
			}
		}
	}
	if host == "" {
		host = r.Host
	}
	if scheme == "" {
		scheme = "https"
	}
	return scheme + "://" + host
}

// handleVerify is called by the reverse proxy's external-auth integration.
// Ingress-agnostic: supports both Traefik forwardAuth (X-Forwarded-Method/-Uri)
// and nginx-ingress auth_request (X-Original-Method + absolute X-Original-URL).
// The original Authorization header is passed through by both proxies.
func handleVerify(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	method, uri := forwardedRoute(r)
	ip := r.Header.Get("X-Forwarded-For")
	authHeader := r.Header.Get("Authorization")

	// Allowlist of unauthenticated paths (k8s probes of the MCP)
	if isPublicPath(uri) {
		logger.Debug("verify_public", "uri", uri, "ip", ip)
		w.WriteHeader(http.StatusOK)
		return
	}

	// No header → 401
	const prefix = "Bearer "
	if !strings.HasPrefix(authHeader, prefix) {
		logger.Info("verify_unauthorized",
			"reason", "missing_bearer",
			"uri", uri,
			"ip", ip,
			"elapsed_ms", time.Since(start).Milliseconds(),
		)
		setChallenge(w, r)
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	tokenStr := strings.TrimPrefix(authHeader, prefix)

	// Validate JWT
	claims := jwt.MapClaims{}
	token, err := jwt.ParseWithClaims(tokenStr, claims, jwks.Keyfunc,
		jwt.WithValidMethods([]string{"RS256"}),
		jwt.WithIssuer(oidcIssuer),
	)
	if err != nil || !token.Valid {
		logger.Info("verify_unauthorized",
			"reason", "jwt_invalid",
			"error", err,
			"uri", uri,
			"ip", ip,
			"elapsed_ms", time.Since(start).Milliseconds(),
		)
		setChallenge(w, r)
		w.WriteHeader(http.StatusUnauthorized)
		return
	}

	// Audience (optional)
	if oidcAud != "" {
		if !claimContains(claims, "aud", oidcAud) {
			logger.Info("verify_unauthorized",
				"reason", "aud_mismatch",
				"uri", uri,
				"ip", ip,
				"elapsed_ms", time.Since(start).Milliseconds(),
			)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
	}

	// Role required by the route
	required := roleForRoute(method, uri)
	if !hasRealmRole(claims, required) {
		sub, _ := claims["sub"].(string)
		logger.Info("verify_forbidden",
			"reason", "missing_role",
			"required_role", required,
			"sub", sub,
			"uri", uri,
			"ip", ip,
			"elapsed_ms", time.Since(start).Milliseconds(),
		)
		w.WriteHeader(http.StatusForbidden)
		return
	}

	sub, _ := claims["sub"].(string)
	logger.Info("verify_ok",
		"sub", sub,
		"role", required,
		"method", method,
		"uri", uri,
		"ip", ip,
		"elapsed_ms", time.Since(start).Milliseconds(),
	)
	// Propagate useful claims to the backend (the MCP server).
	if email, _ := claims["email"].(string); email != "" {
		w.Header().Set("X-Auth-Email", email)
	}
	if username, _ := claims["preferred_username"].(string); username != "" {
		w.Header().Set("X-Auth-Username", username)
	}
	if sub != "" {
		w.Header().Set("X-Auth-Sub", sub)
	}
	// X-Memory-Actor-* — consumed by ai-memory's `crate::actor::scope_from_headers`
	// Tower middleware to populate the per-request `ActorContext` task-local
	// that `memory_write_page` reads when building `admission_ctx`. The
	// admission webhook chain then forwards this actor info to each registered
	// webhook (e.g. `contributors-webhook` uses agent + client as the composite
	// key of its append-only frontmatter set).
	//
	//   preferred_username                            → X-Memory-Actor-User
	//   sub                                           → X-Memory-Actor-Sub
	//   azp  (authorized party / DCR client UUID)     → X-Memory-Actor-Client
	//   client_name  (DCR registered name, optional)  → X-Memory-Actor-Agent
	//                  (falls back to azp when absent so the header is never blank)
	//   sid  (session id, optional)                   → X-Memory-Actor-Session-Id
	if username, _ := claims["preferred_username"].(string); username != "" {
		w.Header().Set("X-Memory-Actor-User", username)
	}
	if sub != "" {
		w.Header().Set("X-Memory-Actor-Sub", sub)
	}
	azp, _ := claims["azp"].(string)
	if azp != "" {
		w.Header().Set("X-Memory-Actor-Client", azp)
	}
	clientName, _ := claims["client_name"].(string)
	if clientName == "" {
		clientName = azp
	}
	if clientName != "" {
		w.Header().Set("X-Memory-Actor-Agent", clientName)
	}
	if sid, _ := claims["sid"].(string); sid != "" {
		w.Header().Set("X-Memory-Actor-Session-Id", sid)
	}
	// Injects the static upstream bearer (ai-memory require_bearer) when configured.
	// Traefik copies this header via authResponseHeaders, swapping the JWT (which ai-memory
	// does not validate) for the static token it expects. Empty = passes the original Authorization.
	if upstreamAuthToken != "" {
		w.Header().Set("Authorization", "Bearer "+upstreamAuthToken)
	}
	w.WriteHeader(http.StatusOK)
}

// isPublicPath allows k8s probes without auth.
func isPublicPath(uri string) bool {
	// Strip query string
	if idx := strings.Index(uri, "?"); idx >= 0 {
		uri = uri[:idx]
	}
	// Allow healthz/readyz + the public OAuth metadata (RFC 9728).
	publics := []string{"/wiki/healthz", "/wiki/readyz", "/healthz", "/readyz"}
	if slices.Contains(publics, uri) {
		return true
	}
	return strings.HasSuffix(uri, "/.well-known/oauth-protected-resource")
}

// roleForRoute decides which realm-level role the route requires.
// Conservative initial policy: everything requires mcp:read; writes (POST/PUT/DELETE/PATCH
// on specific routes) require mcp:write. The write routes will be detailed later.
func roleForRoute(method, uri string) string {
	switch method {
	case http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch:
		// A generic MCP POST is JSON-RPC (tool calls) — read-only for now.
		// Once write routes land, paths like /mcp/write/* will require mcp:write.
		if strings.Contains(uri, "/mcp/write/") || strings.Contains(uri, "/mcp/admin/") {
			return "mcp:write"
		}
	}
	return "mcp:read"
}

// hasRealmRole checks claims['realm_access']['roles'] containing the role.
func hasRealmRole(claims jwt.MapClaims, role string) bool {
	realmAccess, ok := claims["realm_access"].(map[string]any)
	if !ok {
		return false
	}
	roles, ok := realmAccess["roles"].([]any)
	if !ok {
		return false
	}
	for _, r := range roles {
		if s, ok := r.(string); ok && s == role {
			return true
		}
	}
	return false
}

// claimContains handles aud that may be a string or []string.
func claimContains(claims jwt.MapClaims, claim, val string) bool {
	v, ok := claims[claim]
	if !ok {
		return false
	}
	switch t := v.(type) {
	case string:
		return t == val
	case []any:
		for _, item := range t {
			if s, ok := item.(string); ok && s == val {
				return true
			}
		}
	}
	return false
}

// ---- utility helpers ----

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func envRequired(key string) string {
	v := os.Getenv(key)
	if v == "" {
		panic("env var " + key + " is required")
	}
	return v
}

func envIntOr(key string, fallback int) int {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	var n int
	for _, c := range v {
		if c < '0' || c > '9' {
			return fallback
		}
		n = n*10 + int(c-'0')
	}
	if n <= 0 {
		return fallback
	}
	return n
}

func parseLogLevel(s string) slog.Level {
	switch strings.ToLower(s) {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

func decodeJSON(resp *http.Response, v any) error {
	return json.NewDecoder(resp.Body).Decode(v)
}
