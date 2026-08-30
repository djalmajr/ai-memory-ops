// Internal engine client for web-session introspection.
//
// Used only by /keys* when the caller presents an ai_memory_session cookie
// instead of a machine Bearer. The sidecar never validates or claims session
// identity itself: it posts the cookie value to the engine over the Compose
// DNS, with an explicit Host already in AI_MEMORY_ALLOWED_HOSTS, and fails
// closed on any transport, redirect, or protocol error.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

const (
	sessionCookieName     = "ai_memory_session"
	csrfHeaderName        = "X-CSRF-Token"
	nativeKeyPrefix       = "aim_"
	engineInternalTimeout = 2 * time.Second
	engineInternalMaxBody = 1 << 20
	sessionIntrospectPath = "/internal/auth/session-introspect"
	engineComposeHost     = "ai-memory"
	engineInternalPort    = "49374"
)

var (
	errSessionIntrospectFailed = errors.New("session introspection failed")
	errSessionCSRF             = errors.New("csrf required")
	errNativeKeyForbidden      = errors.New("native engine keys cannot manage consumers")
	errEngineRedirect          = errors.New("engine internal redirect refused")
	errEngineInternalMisconfig = errors.New("engine internal client is not configured")

	engineInternalURL    string
	engineInternalHost   string
	engineInternalClient *http.Client
)

type sessionIntrospectRequest struct {
	Session string `json:"session"`
	Method  string `json:"method"`
	CSRF    string `json:"csrf"`
}

type sessionIntrospectResponse struct {
	Authenticated    bool   `json:"authenticated"`
	Username         string `json:"username"`
	CanManageAPIKeys bool   `json:"can_manage_api_keys"`
}

func initEngineInternal() {
	raw := strings.TrimSpace(os.Getenv("ENGINE_INTERNAL_URL"))
	host := strings.TrimSpace(os.Getenv("ENGINE_INTERNAL_HOST"))
	if raw == "" && host == "" {
		return
	}
	normalized, host, err := validateEngineInternalConfig(raw, host)
	if err != nil {
		logger.Error("boot_config_invalid", "reason", err.Error())
		os.Exit(1)
	}
	if actorProxyBearerToken == "" {
		actorProxyBearerToken = os.Getenv("ACTOR_PROXY_BEARER_TOKEN")
	}
	if actorProxyBearerToken == "" {
		logger.Error("actor_proxy_bearer_missing",
			"hint", "ACTOR_PROXY_BEARER_TOKEN is required when ENGINE_INTERNAL_URL is set")
		os.Exit(1)
	}

	engineInternalURL = normalized
	engineInternalHost = host
	engineInternalClient = newEngineInternalClient(engineInternalTimeout)
	logger.Info("engine_internal_enabled",
		"url", engineInternalURL,
		"host", engineInternalHost,
		"timeout_ms", engineInternalTimeout.Milliseconds(),
	)
}

func validateEngineInternalConfig(raw, host string) (string, string, error) {
	raw = strings.TrimSpace(raw)
	host = strings.TrimSpace(host)
	if raw == "" || host == "" {
		return "", "", errors.New("ENGINE_INTERNAL_URL and ENGINE_INTERNAL_HOST must be set together")
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" || u.User != nil {
		return "", "", errors.New("ENGINE_INTERNAL_URL must be an absolute URL without userinfo")
	}
	if u.Scheme != "http" {
		return "", "", errors.New("ENGINE_INTERNAL_URL must use direct http inside Compose")
	}
	if !strings.EqualFold(u.Hostname(), engineComposeHost) || u.Port() != engineInternalPort {
		return "", "", errors.New("ENGINE_INTERNAL_URL must target http://ai-memory:49374")
	}
	if (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
		return "", "", errors.New("ENGINE_INTERNAL_URL must be a bare Compose origin")
	}
	if strings.EqualFold(u.Hostname(), host) {
		return "", "", errors.New("ENGINE_INTERNAL_URL must use Compose DNS, not ENGINE_INTERNAL_HOST")
	}
	return "http://" + engineComposeHost + ":" + engineInternalPort, host, nil
}

func newEngineInternalClient(timeout time.Duration) *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	return &http.Client{
		Transport: transport,
		Timeout:   timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return errEngineRedirect
		},
	}
}

func isMutatingMethod(method string) bool {
	switch method {
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		return true
	default:
		return false
	}
}

func introspectSession(session, method, csrf string) (*sessionIntrospectResponse, error) {
	if engineInternalClient == nil || engineInternalURL == "" || engineInternalHost == "" {
		return nil, errEngineInternalMisconfig
	}
	payload, err := json.Marshal(sessionIntrospectRequest{
		Session: session,
		Method:  method,
		CSRF:    csrf,
	})
	if err != nil {
		return nil, errSessionIntrospectFailed
	}

	ctx, cancel := context.WithTimeout(context.Background(), engineInternalTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, engineInternalURL+sessionIntrospectPath, bytes.NewReader(payload))
	if err != nil {
		return nil, errSessionIntrospectFailed
	}
	req.Host = engineInternalHost
	req.Header.Set("Authorization", "Bearer "+actorProxyBearerToken)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	for _, h := range []string{
		"X-Memory-Actor-User",
		"X-Memory-Actor-Sub",
		"X-Memory-Actor-Issuer",
		"X-Memory-Actor-Client",
		"X-Memory-Actor-Agent",
		"X-Memory-Actor-Session-Id",
		"Cookie",
	} {
		req.Header.Del(h)
	}

	start := time.Now()
	resp, err := engineInternalClient.Do(req)
	if err != nil {
		logger.Error("session_introspect_failed",
			"reason", "transport",
			"elapsed_ms", time.Since(start).Milliseconds(),
		)
		return nil, errSessionIntrospectFailed
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, engineInternalMaxBody))
	if err != nil {
		logger.Error("session_introspect_failed",
			"reason", "read",
			"status", resp.StatusCode,
			"elapsed_ms", time.Since(start).Milliseconds(),
		)
		return nil, errSessionIntrospectFailed
	}
	if resp.StatusCode != http.StatusOK {
		logger.Error("session_introspect_failed",
			"reason", "status",
			"status", resp.StatusCode,
			"elapsed_ms", time.Since(start).Milliseconds(),
		)
		if resp.StatusCode == http.StatusForbidden {
			return nil, errSessionCSRF
		}
		return nil, errSessionIntrospectFailed
	}

	var out sessionIntrospectResponse
	if err := json.Unmarshal(body, &out); err != nil {
		logger.Error("session_introspect_failed",
			"reason", "json",
			"elapsed_ms", time.Since(start).Milliseconds(),
		)
		return nil, errSessionIntrospectFailed
	}
	if out.Authenticated && strings.TrimSpace(out.Username) == "" {
		logger.Error("session_introspect_failed",
			"reason", "empty_username",
			"elapsed_ms", time.Since(start).Milliseconds(),
		)
		return nil, errSessionIntrospectFailed
	}
	logger.Info("session_introspect_ok",
		"authenticated", out.Authenticated,
		"can_manage_api_keys", out.CanManageAPIKeys,
		"elapsed_ms", time.Since(start).Milliseconds(),
	)
	return &out, nil
}
