// scope-guard — admission webhook for ai-memory.
//
// Engine POSTs { page, ctx } to /admit on every write/delete/consolidate.
// scope-guard checks `(ctx.actor.user, ctx.workspace, ctx.project)` against
// an operator-supplied ACL: each user maps to a list of (workspace,
// project) regex pairs. If at least one pair matches, the response is
// `200 OK` (allow). Otherwise the response is `403 Forbidden` with a
// reason string — the engine's admission chain (with `failure_policy:
// reject`) aborts the write atomically before disk/DB.
//
// Ops that relocate content (`move_project`, `move_session`) are checked
// against BOTH scopes: the source `(ctx.workspace, ctx.project)` and the
// destination `(ctx.destination_workspace, ctx.destination_project)`. A
// caller admitted only at the source could otherwise push content into a
// workspace/project it cannot write to directly. A move that arrives with
// no usable destination is rejected (fail-closed) — see `evaluate`.
//
// This is a write-side ACL, not a read-side one — the engine has no
// read admission chain. For real read privacy, run separate ai-memory
// instances.
//
// Rules are loaded from the ACL_RULES env var, a JSON object of shape
//
//	{
//	  "alice":     [{ "workspace": "alice|shared", "project": ".*" }],
//	  "bob":       [{ "workspace": "bob|shared",   "project": ".*" }],
//	  "djalmajr-foo": [{ "workspace": "djalmajr",  "project": "foo" }]
//	}
//
// Both workspace and project are anchored with `^…$` automatically;
// callers can use `|` for alternation or `.*` for "anything".
//
// Special key `*` matches any user (useful for shared/read-only scopes
// or a temporary "permit everyone in this workspace" policy). The
// `failure_policy: reject` decision is the engine's, NOT this webhook's.
// scope-guard's job is to return `200` (allow) or `403` (reject) — the
// chart wires `failure_policy: reject` when scope-guard is enabled.
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
	"regexp"
	"syscall"
	"time"
)

type payload struct {
	Page json.RawMessage `json:"page"`
	Ctx  struct {
		// Workspace/Project are the SOURCE scope. For every op except the
		// moves they are the only scope there is; for a move they name where
		// the content is coming FROM.
		Workspace string `json:"workspace"`
		Project   string `json:"project"`
		// DestinationWorkspace/DestinationProject name where a move op puts
		// the content. They mirror the engine's
		// `AdmissionContext::destination_{workspace,project}`, both
		// `Option<String>` serialised with `skip_serializing_if =
		// "Option::is_none"` — so on a non-move op the keys are absent from
		// the JSON entirely rather than sent as `null`.
		//
		// Pointers rather than plain strings: a plain `string` collapses
		// "the engine never sent this field" and "the engine sent an empty
		// name" into the same `""`. Both are refused for a move (see
		// evaluate), so the distinction never changes the decision — but it
		// is the whole difference between "engine predates the field / is
		// not wiring it" and "engine sent a broken scope" when an operator
		// triages a move that suddenly started 403-ing, and the reject log
		// can only say which one happened if the parse kept them apart.
		DestinationWorkspace *string `json:"destination_workspace"`
		DestinationProject   *string `json:"destination_project"`
		Op                   string  `json:"op"`
		Actor                struct {
			User string `json:"user"`
		} `json:"actor"`
	} `json:"ctx"`
}

type ruleSpec struct {
	Workspace string `json:"workspace"`
	Project   string `json:"project"`
	// Ops narrows a rule to specific admission operations (`consolidate`,
	// `write_page`, `delete`, …). Omitted or empty = every gated op, which
	// is what every rule written before this field existed means.
	//
	// It exists so a rule can admit the SERVER's own unattributed work —
	// scheduled lint/consolidation carries no actor by design — without
	// also handing an unidentified caller `delete` and `purge_project`.
	Ops []string `json:"ops,omitempty"`
}

type compiledRule struct {
	Workspace *regexp.Regexp
	Project   *regexp.Regexp
	// nil = the rule applies to every op; otherwise a membership set.
	Ops map[string]struct{}
}

func compile(rules map[string][]ruleSpec) (map[string][]compiledRule, error) {
	out := make(map[string][]compiledRule, len(rules))
	for user, specs := range rules {
		for _, spec := range specs {
			ws, err := regexp.Compile("^(?:" + spec.Workspace + ")$")
			if err != nil {
				return nil, &compileErr{User: user, Field: "workspace", Pattern: spec.Workspace, Err: err}
			}
			proj, err := regexp.Compile("^(?:" + spec.Project + ")$")
			if err != nil {
				return nil, &compileErr{User: user, Field: "project", Pattern: spec.Project, Err: err}
			}
			var ops map[string]struct{}
			if len(spec.Ops) > 0 {
				ops = make(map[string]struct{}, len(spec.Ops))
				for _, op := range spec.Ops {
					ops[op] = struct{}{}
				}
			}
			out[user] = append(out[user], compiledRule{Workspace: ws, Project: proj, Ops: ops})
		}
	}
	return out, nil
}

type compileErr struct {
	User    string
	Field   string
	Pattern string
	Err     error
}

func (e *compileErr) Error() string {
	return "scope-guard: invalid " + e.Field + " regex for user " + e.User + ": " + e.Pattern + " — " + e.Err.Error()
}

// admitted decides whether (user, workspace, project) is allowed by the
// rule set. The wildcard user key `*` applies to every caller — its
// rules are evaluated in addition to the user-specific ones.
func admitted(rules map[string][]compiledRule, user, workspace, project, op string) bool {
	for _, key := range []string{user, "*"} {
		for _, r := range rules[key] {
			if !r.Workspace.MatchString(workspace) || !r.Project.MatchString(project) {
				continue
			}
			if r.Ops == nil {
				return true
			}
			if _, ok := r.Ops[op]; ok {
				return true
			}
		}
	}
	return false
}

// isMoveOp reports whether an admission op relocates content, i.e. whether it
// carries a destination scope that must be authorised on top of the source
// one. Deliberately an explicit allow-list: a future relocating op that isn't
// listed here is still checked against its source scope exactly as today —
// it just doesn't get the extra destination check until someone adds it.
func isMoveOp(op string) bool {
	switch op {
	case "move_project", "move_session":
		return true
	}
	return false
}

// scopeOf derefs an optional ctx field. A nil pointer (key absent) and an
// empty string (key present but blank) are equally unusable as a scope, so
// they collapse here; describeScope keeps them apart for the log.
func scopeOf(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// describeScope renders an optional ctx field for logs/errors, distinguishing
// "the key wasn't in the payload" from "the key was there and empty".
func describeScope(p *string) string {
	switch {
	case p == nil:
		return "absent"
	case *p == "":
		return "empty"
	default:
		return quote(*p)
	}
}

// evaluate applies the ACL to one parsed request and returns the decision plus,
// on a reject, a reason naming the side that failed.
//
// Non-move ops are checked exactly as they always were: one `admitted` call on
// (user, workspace, project, op).
//
// Move ops (`move_project`, `move_session`) must satisfy the ACL on BOTH ends.
// Source-only checking — what this webhook did before — lets any caller
// admitted somewhere relocate content into a workspace/project it has no write
// rights to at all, which is a strictly larger capability than the writes the
// ACL grants it: the destination is where the content actually lands, so the
// destination scope has to authorise it too.
//
// A move whose destination is absent or empty is DENIED, and there is no knob
// to change that. Fail-open here would restore the exact hole being closed,
// and it would be reachable by omitting a field — i.e. controlled by the
// caller, not the operator. It also costs nothing in practice: the engine
// constructs both move contexts with `destination_{workspace,project}:
// Some(...)` unconditionally (crates/ai-memory-mcp/src/admin.rs, move_project
// and move_session), and the fields landed in the same commits as the ops
// themselves, so no engine version can emit a move without them. A move that
// arrives here with no destination is a forged payload or an engine
// regression, and in both cases "I can't tell where this lands" must not read
// as "allowed".
func evaluate(rules map[string][]compiledRule, user, ws, proj, op string, destWS, destProj *string) (bool, string) {
	move := isMoveOp(op)
	if !admitted(rules, user, ws, proj, op) {
		if move {
			return false, "scope-guard: user " + quote(user) + " not allowed to " + op +
				" out of " + quote(ws) + "/" + quote(proj) + " (source scope)"
		}
		return false, "scope-guard: user " + quote(user) + " not allowed to " + op +
			" in " + quote(ws) + "/" + quote(proj)
	}
	if !move {
		return true, ""
	}
	dws, dproj := scopeOf(destWS), scopeOf(destProj)
	if dws == "" || dproj == "" {
		return false, "scope-guard: " + op + " out of " + quote(ws) + "/" + quote(proj) +
			" denied: unusable destination scope (destination_workspace=" + describeScope(destWS) +
			", destination_project=" + describeScope(destProj) + ") — move ops fail closed"
	}
	if !admitted(rules, user, dws, dproj, op) {
		return false, "scope-guard: user " + quote(user) + " not allowed to " + op +
			" into " + quote(dws) + "/" + quote(dproj) + " (destination scope)"
	}
	return true, ""
}

func loadRules() (map[string][]compiledRule, error) {
	raw := os.Getenv("ACL_RULES")
	if raw == "" {
		// No ACL configured = deny-by-default, with a startup warning so
		// the operator notices. Returning an empty map (rather than
		// erroring out) lets the pod stay up while ACL is being staged
		// — but every write will be rejected until rules land.
		return map[string][]compiledRule{}, nil
	}
	var rules map[string][]ruleSpec
	if err := json.Unmarshal([]byte(raw), &rules); err != nil {
		return nil, err
	}
	return compile(rules)
}

func handler(logger *slog.Logger, rules map[string][]compiledRule) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		// 1 MiB cap mirrors the engine's MAX_RESPONSE_BYTES — we never
		// need more than the page envelope to make a decision.
		const maxBody = 1 << 20
		body, err := io.ReadAll(io.LimitReader(r.Body, maxBody))
		if err != nil {
			http.Error(w, "read body: "+err.Error(), http.StatusBadRequest)
			return
		}
		var p payload
		if err := json.Unmarshal(body, &p); err != nil {
			http.Error(w, "parse payload: "+err.Error(), http.StatusBadRequest)
			return
		}
		user := p.Ctx.Actor.User
		ws := p.Ctx.Workspace
		proj := p.Ctx.Project
		attrs := []any{"user", user, "workspace", ws, "project", proj, "op", p.Ctx.Op}
		// Only moves carry a destination; logging it on every write_page
		// would be constant "absent" noise.
		if isMoveOp(p.Ctx.Op) {
			attrs = append(attrs,
				"destination_workspace", describeScope(p.Ctx.DestinationWorkspace),
				"destination_project", describeScope(p.Ctx.DestinationProject))
		}
		allow, reason := evaluate(rules, user, ws, proj, p.Ctx.Op,
			p.Ctx.DestinationWorkspace, p.Ctx.DestinationProject)
		if allow {
			logger.Debug("admit", attrs...)
			w.Header().Set("content-type", "application/json")
			// Empty body = "allow, no page mutation"; the engine treats
			// missing `page.frontmatter` / `page.body` as unchanged.
			_, _ = w.Write([]byte(`{}`))
			return
		}
		logger.Info("reject", append(attrs, "reason", reason)...)
		w.Header().Set("content-type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": reason})
	}
}

// quote wraps a user-supplied string in single quotes for log/error
// readability without invoking %q's escape semantics (the values are
// already validated by the engine; this is purely display).
func quote(s string) string {
	if s == "" {
		return "''"
	}
	return "'" + s + "'"
}

func main() {
	level := slog.LevelInfo
	if v := os.Getenv("LOG_LEVEL"); v != "" {
		switch v {
		case "debug":
			level = slog.LevelDebug
		case "warn":
			level = slog.LevelWarn
		case "error":
			level = slog.LevelError
		}
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
	rules, err := loadRules()
	if err != nil {
		logger.Error("load rules", "error", err)
		os.Exit(2)
	}
	if len(rules) == 0 {
		logger.Warn("scope-guard started with NO rules; every write will be rejected until ACL_RULES is set")
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/admit", handler(logger, rules))
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	addr := os.Getenv("LISTEN_ADDR")
	if addr == "" {
		addr = ":8080"
	}
	srv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		// Idle-timeout matters for the engine's reqwest client which
		// keeps connections warm between writes.
		IdleTimeout: 60 * time.Second,
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go func() {
		logger.Info("scope-guard listening", "addr", addr, "users_configured", len(rules))
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("listen", "error", err)
			os.Exit(1)
		}
	}()
	<-ctx.Done()
	shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutCtx)
}
