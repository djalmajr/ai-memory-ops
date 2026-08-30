package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func setEngineInternalForTest(t *testing.T, baseURL, host string, timeout time.Duration) {
	t.Helper()
	if timeout <= 0 {
		timeout = engineInternalTimeout
	}
	engineInternalURL = strings.TrimRight(baseURL, "/")
	engineInternalHost = host
	engineInternalClient = newEngineInternalClient(timeout)
	t.Cleanup(func() {
		engineInternalURL = ""
		engineInternalHost = ""
		engineInternalClient = nil
	})
}

func TestValidateEngineInternalConfig(t *testing.T) {
	invalid := []string{
		"http://ai-memory:8080",
		"http://memory.example.test:49374",
		"http://user:pass@ai-memory:49374",
		"https://ai-memory:49374",
		"http://engine:49374",
		"http://ai-memory",
		"http://ai-memory:49374/internal",
		"http://ai-memory:49374?target=other",
		"http://ai-memory:49374#fragment",
	}
	for _, raw := range invalid {
		if _, _, err := validateEngineInternalConfig(raw, "memory.example.test"); err == nil {
			t.Errorf("validateEngineInternalConfig(%q) unexpectedly succeeded", raw)
		}
	}

	got, host, err := validateEngineInternalConfig("http://ai-memory:49374/", "memory.example.test")
	if err != nil {
		t.Fatal(err)
	}
	if got != "http://ai-memory:49374" || host != "memory.example.test" {
		t.Fatalf("got url=%q host=%q", got, host)
	}
}

func TestEngineInternalClientDisablesEnvironmentProxy(t *testing.T) {
	client := newEngineInternalClient(time.Second)
	transport, ok := client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport type = %T, want *http.Transport", client.Transport)
	}
	if transport.Proxy != nil {
		t.Fatal("internal engine transport must not consult proxy environment variables")
	}
}

func TestIntrospectSetsHostAndOmitsActorHeaders(t *testing.T) {
	var gotHost string
	var gotAuth string
	var gotActors []string
	var body sessionIntrospectRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHost = r.Host
		gotAuth = r.Header.Get("Authorization")
		for _, h := range []string{
			"X-Memory-Actor-User", "X-Memory-Actor-Sub", "X-Memory-Actor-Issuer",
			"X-Memory-Actor-Client", "X-Memory-Actor-Agent", "Cookie",
		} {
			if r.Header.Get(h) != "" {
				gotActors = append(gotActors, h)
			}
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"authenticated":true,"username":"root","can_manage_api_keys":true}`)
	}))
	t.Cleanup(srv.Close)
	actorProxyBearerToken = "proxy-bearer"
	t.Cleanup(func() { actorProxyBearerToken = "" })
	setEngineInternalForTest(t, srv.URL, "memory.example.test", 0)

	out, err := introspectSession("ams_secret", http.MethodPost, "csrf-token")
	if err != nil {
		t.Fatalf("introspectSession: %v", err)
	}
	if !out.Authenticated || out.Username != "root" || !out.CanManageAPIKeys {
		t.Fatalf("result = %+v", out)
	}
	if gotHost != "memory.example.test" {
		t.Errorf("Host = %q, want ENGINE_INTERNAL_HOST", gotHost)
	}
	if gotAuth != "Bearer proxy-bearer" {
		t.Errorf("Authorization = %q, want actor-proxy bearer", gotAuth)
	}
	if len(gotActors) != 0 {
		t.Errorf("actor/cookie headers leaked: %v", gotActors)
	}
	if body.Session != "ams_secret" || body.Method != http.MethodPost || body.CSRF != "csrf-token" {
		t.Errorf("payload = %+v", body)
	}
}

func TestIntrospectFailsClosedOnRedirect(t *testing.T) {
	final := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"authenticated":true,"username":"root","can_manage_api_keys":true}`)
	}))
	t.Cleanup(final.Close)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, final.URL, http.StatusFound)
	}))
	t.Cleanup(srv.Close)
	actorProxyBearerToken = "proxy-bearer"
	t.Cleanup(func() { actorProxyBearerToken = "" })
	setEngineInternalForTest(t, srv.URL, "memory.example.test", 0)

	if _, err := introspectSession("ams_secret", http.MethodGet, ""); err == nil {
		t.Fatal("redirect must fail closed")
	}
}

func TestIntrospectFailsClosedOnTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(80 * time.Millisecond)
		_, _ = io.WriteString(w, `{"authenticated":true,"username":"root","can_manage_api_keys":true}`)
	}))
	t.Cleanup(srv.Close)
	actorProxyBearerToken = "proxy-bearer"
	t.Cleanup(func() { actorProxyBearerToken = "" })
	setEngineInternalForTest(t, srv.URL, "memory.example.test", 20*time.Millisecond)

	if _, err := introspectSession("ams_secret", http.MethodGet, ""); err == nil {
		t.Fatal("timeout must fail closed")
	}
}

func TestIntrospectFailsClosedOnInvalidJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `not-json`)
	}))
	t.Cleanup(srv.Close)
	actorProxyBearerToken = "proxy-bearer"
	t.Cleanup(func() { actorProxyBearerToken = "" })
	setEngineInternalForTest(t, srv.URL, "memory.example.test", 0)

	if _, err := introspectSession("ams_secret", http.MethodGet, ""); err == nil {
		t.Fatal("invalid JSON must fail closed")
	}
}

func TestIntrospectFailsClosedOnEmptyAuthenticatedUsername(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"authenticated":true,"username":"","can_manage_api_keys":true}`)
	}))
	t.Cleanup(srv.Close)
	actorProxyBearerToken = "proxy-bearer"
	t.Cleanup(func() { actorProxyBearerToken = "" })
	setEngineInternalForTest(t, srv.URL, "memory.example.test", 0)

	if _, err := introspectSession("ams_secret", http.MethodGet, ""); err == nil {
		t.Fatal("authenticated response without username must fail closed")
	}
}
