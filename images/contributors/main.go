// contributors — admission webhook for ai-memory.
//
// POST /enrich receives { page, ctx } from the engine's admission chain
// and appends/updates an entry in `frontmatter.contributors`, an
// append-only set keyed by (agent, client). Existing user/sub fields are
// preserved when present (so a relogin with new claims doesn't strip
// info captured earlier).
//
// Wire:
//   request:  { page: { path, frontmatter, body }, ctx: { actor: {agent,user,sub,client,session_id} } }
//   200:      { page: { frontmatter } }            — actor identified
//   204:      empty                                 — actor incomplete, page unchanged
package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

type payload struct {
	Page struct {
		Path        string         `json:"path"`
		Frontmatter map[string]any `json:"frontmatter"`
		Body        string         `json:"body"`
	} `json:"page"`
	Ctx struct {
		Actor struct {
			Agent     string `json:"agent"`
			User      string `json:"user"`
			Sub       string `json:"sub"`
			Client    string `json:"client"`
			SessionID string `json:"session_id"`
		} `json:"actor"`
	} `json:"ctx"`
}

// enrich is exported as a function (not handler) to make it directly
// testable without spinning up an httptest server.
func enrich(in payload) (mutated map[string]any, ok bool) {
	a := in.Ctx.Actor
	// No (agent, client) → nothing to attribute. 204 lets the engine
	// skip the mutation entirely.
	if a.Agent == "" || a.Client == "" {
		return nil, false
	}

	fm := in.Page.Frontmatter
	if fm == nil {
		fm = map[string]any{}
	}

	existing, _ := fm["contributors"].([]any)
	now := time.Now().UTC().Format(time.RFC3339Nano)
	key := a.Agent + "|" + a.Client
	updated := false

	out := make([]any, 0, len(existing)+1)
	for _, e := range existing {
		entry, isObj := e.(map[string]any)
		if !isObj {
			out = append(out, e)
			continue
		}
		entryKey := str(entry["agent"]) + "|" + str(entry["client"])
		if entryKey != key {
			out = append(out, entry)
			continue
		}
		updated = true
		entry["last_seen"] = now
		entry["writes"] = writesOf(entry["writes"]) + 1
		// Don't churn user/sub if already populated.
		if _, has := entry["user"]; !has && a.User != "" {
			entry["user"] = a.User
		}
		if _, has := entry["sub"]; !has && a.Sub != "" {
			entry["sub"] = a.Sub
		}
		out = append(out, entry)
	}

	if !updated {
		entry := map[string]any{
			"agent":      a.Agent,
			"client":     a.Client,
			"first_seen": now,
			"last_seen":  now,
			"writes":     int64(1),
		}
		if a.User != "" {
			entry["user"] = a.User
		} else {
			entry["user"] = nil
		}
		if a.Sub != "" {
			entry["sub"] = a.Sub
		} else {
			entry["sub"] = nil
		}
		out = append(out, entry)
	}

	fm["contributors"] = out
	return fm, true
}

func writesOf(v any) int64 {
	switch n := v.(type) {
	case float64:
		return int64(n)
	case int64:
		return n
	case int:
		return int64(n)
	case json.Number:
		i, _ := n.Int64()
		return i
	}
	return 0
}

func str(v any) string {
	s, _ := v.(string)
	return s
}

func handleEnrich(w http.ResponseWriter, r *http.Request) {
	defer r.Body.Close()
	body, err := io.ReadAll(io.LimitReader(r.Body, 4<<20)) // 4 MiB cap
	if err != nil {
		http.Error(w, "read body: "+err.Error(), http.StatusBadRequest)
		return
	}
	var in payload
	if err := json.Unmarshal(body, &in); err != nil {
		http.Error(w, "decode: "+err.Error(), http.StatusBadRequest)
		return
	}
	fm, ok := enrich(in)
	if !ok {
		slog.Debug("actor incomplete, skipping", "path", in.Page.Path)
		w.WriteHeader(http.StatusNoContent)
		return
	}
	out := map[string]any{"page": map[string]any{"frontmatter": fm}}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

func handleHealth(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok\n"))
}

func main() {
	addr := os.Getenv("LISTEN_ADDR")
	if addr == "" {
		addr = "0.0.0.0:8080"
	}
	setupLogger()

	mux := http.NewServeMux()
	mux.HandleFunc("POST /enrich", handleEnrich)
	mux.HandleFunc("GET /healthz", handleHealth)

	srv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	go func() {
		slog.Info("contributors listening", "addr", addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("server error", "err", err)
			os.Exit(1)
		}
	}()

	<-ctx.Done()
	slog.Info("shutdown signal received")
	shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutCtx)
}

func setupLogger() {
	level := slog.LevelInfo
	switch os.Getenv("LOG_LEVEL") {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	}
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level})))
}

