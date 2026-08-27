package main

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// helper: compile a rule set from a JSON-shaped map (mirrors the
// production startup path).
func mustCompile(t *testing.T, src map[string][]ruleSpec) map[string][]compiledRule {
	t.Helper()
	out, err := compile(src)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	return out
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func postAdmit(t *testing.T, h http.Handler, body any) (int, string) {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/admit", bytes.NewReader(raw))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code, rec.Body.String()
}

func payloadFor(user, ws, proj, op string) map[string]any {
	return map[string]any{
		"page": map[string]any{"path": "irrelevant", "frontmatter": map[string]any{}, "body": ""},
		"ctx": map[string]any{
			"workspace": ws,
			"project":   proj,
			"op":        op,
			"actor":     map[string]any{"user": user},
		},
	}
}

func TestAdmittedExactMatch(t *testing.T) {
	rules := mustCompile(t, map[string][]ruleSpec{
		"alice": {{Workspace: "alice", Project: "notes"}},
	})
	if !admitted(rules, "alice", "alice", "notes", "write_page") {
		t.Fatal("alice should be allowed in alice/notes")
	}
	if admitted(rules, "alice", "alice", "secrets", "write_page") {
		t.Fatal("alice/secrets must be rejected — project doesn't match")
	}
	if admitted(rules, "bob", "alice", "notes", "write_page") {
		t.Fatal("bob in alice/notes must be rejected — user not in rules")
	}
}

func TestAdmittedRegexUnion(t *testing.T) {
	rules := mustCompile(t, map[string][]ruleSpec{
		"alice": {{Workspace: "alice|shared", Project: ".*"}},
	})
	if !admitted(rules, "alice", "shared", "any", "write_page") {
		t.Fatal("alice should be allowed in shared/any via union")
	}
	if !admitted(rules, "alice", "alice", "deep/path/x.md", "write_page") {
		t.Fatal("alice should be allowed in alice/deep — .* matches everything")
	}
	if admitted(rules, "alice", "djalmajr", "notes", "write_page") {
		t.Fatal("alice in djalmajr/* must be rejected — workspace doesn't match union")
	}
}

func TestAdmittedWildcardUser(t *testing.T) {
	rules := mustCompile(t, map[string][]ruleSpec{
		"*":     {{Workspace: "public", Project: ".*"}},
		"alice": {{Workspace: "alice", Project: ".*"}},
	})
	// Bob has no specific rule but `*` covers public.
	if !admitted(rules, "bob", "public", "anything", "write_page") {
		t.Fatal("wildcard rule must apply to all users")
	}
	// Alice still gets her own rules even when * exists.
	if !admitted(rules, "alice", "alice", "x", "write_page") {
		t.Fatal("user-specific rule must still apply alongside *")
	}
	// Wildcard does NOT cover private workspaces.
	if admitted(rules, "bob", "alice", "x", "write_page") {
		t.Fatal("wildcard must not grant access to private workspaces")
	}
}

func TestAdmittedAnchored(t *testing.T) {
	rules := mustCompile(t, map[string][]ruleSpec{
		"alice": {{Workspace: "alice", Project: "notes"}},
	})
	// A naive "contains" check would allow alice-evil / notes-evil;
	// the auto-anchor `^…$` prevents that.
	if admitted(rules, "alice", "alice-evil", "notes", "write_page") {
		t.Fatal("alice-evil must be rejected — regex must be anchored")
	}
	if admitted(rules, "alice", "alice", "notes-secret", "write_page") {
		t.Fatal("notes-secret must be rejected — regex must be anchored")
	}
}

func TestHandlerAllowsMatchingScope(t *testing.T) {
	rules := mustCompile(t, map[string][]ruleSpec{
		"alice": {{Workspace: "alice|shared", Project: ".*"}},
	})
	h := handler(discardLogger(), rules)
	code, body := postAdmit(t, h, payloadFor("alice", "shared", "team-docs", "write_page"))
	if code != http.StatusOK {
		t.Fatalf("expected 200, got %d (%s)", code, body)
	}
	if body != "{}" {
		t.Fatalf("allow body must be empty JSON object, got %q", body)
	}
}

func TestHandlerRejectsForeignScope(t *testing.T) {
	rules := mustCompile(t, map[string][]ruleSpec{
		"alice": {{Workspace: "alice", Project: ".*"}},
	})
	h := handler(discardLogger(), rules)
	code, body := postAdmit(t, h, payloadFor("alice", "djalmajr", "notes", "write_page"))
	if code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d (%s)", code, body)
	}
	if !bytes.Contains([]byte(body), []byte("not allowed to write_page")) {
		t.Fatalf("reject body must explain the op, got %q", body)
	}
}

func TestHandlerEmptyRulesRejectsEverything(t *testing.T) {
	// Operator started the pod without ACL_RULES — deny-by-default,
	// no panic.
	h := handler(discardLogger(), map[string][]compiledRule{})
	code, _ := postAdmit(t, h, payloadFor("alice", "alice", "notes", "write_page"))
	if code != http.StatusForbidden {
		t.Fatalf("empty rule set must reject all writes, got %d", code)
	}
}

func TestHandlerRejectsNonPost(t *testing.T) {
	h := handler(discardLogger(), map[string][]compiledRule{})
	req := httptest.NewRequest(http.MethodGet, "/admit", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET must 405, got %d", rec.Code)
	}
}

func TestCompileSurfacesBadPattern(t *testing.T) {
	_, err := compile(map[string][]ruleSpec{
		"alice": {{Workspace: "[unterminated", Project: ".*"}},
	})
	if err == nil {
		t.Fatal("malformed regex must surface as compile error at startup")
	}
}

// A rule without `ops` keeps its historical meaning — every gated operation —
// so existing ACLs behave identically after the field was introduced.
func TestRuleWithoutOpsAppliesToEveryOp(t *testing.T) {
	rules, err := compile(map[string][]ruleSpec{
		"alice": {{Workspace: "alice", Project: ".*"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, op := range []string{"write_page", "consolidate", "delete", "purge_project"} {
		if !admitted(rules, "alice", "alice", "notes", op) {
			t.Errorf("alice should be allowed to %s", op)
		}
	}
}

// The case this field exists for: the server's own scheduled lint/consolidation
// carries no actor, so it arrives as the empty user. Admitting it must NOT also
// hand an unidentified caller the destructive operations.
func TestEmptyUserCanBeLimitedToConsolidate(t *testing.T) {
	rules, err := compile(map[string][]ruleSpec{
		"":      {{Workspace: ".*", Project: ".*", Ops: []string{"consolidate"}}},
		"alice": {{Workspace: "alice", Project: ".*"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !admitted(rules, "", "anything", "anywhere", "consolidate") {
		t.Fatal("the scheduler's unattributed consolidate must be admitted")
	}
	for _, op := range []string{"write_page", "delete", "purge_project", "move_project"} {
		if admitted(rules, "", "anything", "anywhere", op) {
			t.Errorf("unattributed %s must stay rejected", op)
		}
	}
	// A named user's own rule is unaffected by the empty-user entry.
	if !admitted(rules, "alice", "alice", "notes", "delete") {
		t.Fatal("alice's unscoped rule should still cover every op")
	}
}

// ---------------------------------------------------------------------------
// Move ops: destination scope must be admitted too.
// ---------------------------------------------------------------------------

func strp(s string) *string { return &s }

// payloadWithDest is payloadFor plus the destination keys a move op carries.
// A nil pointer omits the key entirely, which is how the engine serialises
// `Option::None` (`skip_serializing_if = "Option::is_none"`).
func payloadWithDest(user, ws, proj, op string, destWS, destProj *string) map[string]any {
	p := payloadFor(user, ws, proj, op)
	ctx := p["ctx"].(map[string]any)
	if destWS != nil {
		ctx["destination_workspace"] = *destWS
	}
	if destProj != nil {
		ctx["destination_project"] = *destProj
	}
	return p
}

// prodACL is the ACL running in production, verbatim, parsed from the same
// JSON shape the ACL_RULES env var carries — so a change to rule semantics
// that would alter real behaviour fails here.
const prodACL = `{"":[{"ops":["consolidate","delete"],"project":".*","workspace":".*"}],` +
	`"djalmajr":[{"project":".*","workspace":".*"}]}`

func mustCompileJSON(t *testing.T, raw string) map[string][]compiledRule {
	t.Helper()
	var specs map[string][]ruleSpec
	if err := json.Unmarshal([]byte(raw), &specs); err != nil {
		t.Fatalf("unmarshal ACL: %v", err)
	}
	return mustCompile(t, specs)
}

func TestEvaluateMoveChecksBothScopes(t *testing.T) {
	// alice may write anywhere in `alice`; bob's workspace is off limits.
	// carol may move out of `alice` but only into `carol`.
	acl := map[string][]ruleSpec{
		"alice": {{Workspace: "alice", Project: ".*"}},
		"carol": {
			{Workspace: "alice", Project: ".*", Ops: []string{"move_project", "move_session"}},
			{Workspace: "carol", Project: ".*"},
		},
	}

	cases := []struct {
		name       string
		user       string
		ws, proj   string
		op         string
		destWS     *string
		destProj   *string
		wantAllow  bool
		wantReason string // substring; empty when the case must be allowed
	}{
		{
			name: "move allowed on both sides",
			user: "alice", ws: "alice", proj: "notes", op: "move_project",
			destWS: strp("alice"), destProj: strp("notes"),
			wantAllow: true,
		},
		{
			name: "move_session allowed on both sides",
			user: "alice", ws: "alice", proj: "notes", op: "move_session",
			destWS: strp("alice"), destProj: strp("archive"),
			wantAllow: true,
		},
		{
			// The hole this closes: alice is admitted at the source and would
			// have passed the old source-only check while relocating content
			// into a workspace she cannot write to.
			name: "denied on destination only",
			user: "alice", ws: "alice", proj: "notes", op: "move_project",
			destWS: strp("bob"), destProj: strp("notes"),
			wantAllow: false, wantReason: "into 'bob'/'notes' (destination scope)",
		},
		{
			name: "denied on destination project only",
			user: "carol", ws: "alice", proj: "notes", op: "move_session",
			destWS: strp("bob"), destProj: strp("notes"),
			wantAllow: false, wantReason: "(destination scope)",
		},
		{
			name: "denied on source only",
			user: "alice", ws: "bob", proj: "notes", op: "move_project",
			destWS: strp("alice"), destProj: strp("notes"),
			wantAllow: false, wantReason: "out of 'bob'/'notes' (source scope)",
		},
		{
			// Source is evaluated first, so a doubly-invalid move reports the
			// source — the operator fixes that end before the other matters.
			name: "denied on both sides reports source",
			user: "alice", ws: "bob", proj: "notes", op: "move_project",
			destWS: strp("dave"), destProj: strp("notes"),
			wantAllow: false, wantReason: "(source scope)",
		},
		{
			name: "destination workspace absent on a move is fail-closed",
			user: "alice", ws: "alice", proj: "notes", op: "move_project",
			destWS: nil, destProj: strp("notes"),
			wantAllow: false, wantReason: "destination_workspace=absent",
		},
		{
			name: "destination project absent on a move is fail-closed",
			user: "alice", ws: "alice", proj: "notes", op: "move_session",
			destWS: strp("alice"), destProj: nil,
			wantAllow: false, wantReason: "destination_project=absent",
		},
		{
			name: "both destination fields absent on a move is fail-closed",
			user: "alice", ws: "alice", proj: "notes", op: "move_project",
			destWS: nil, destProj: nil,
			wantAllow: false, wantReason: "move ops fail closed",
		},
		{
			// An empty name is as unusable as a missing one, but the log must
			// distinguish them so the operator knows which failure this was.
			name: "empty destination on a move is fail-closed and logged as empty",
			user: "alice", ws: "alice", proj: "notes", op: "move_project",
			destWS: strp(""), destProj: strp("notes"),
			wantAllow: false, wantReason: "destination_workspace=empty",
		},
		{
			// Regression guard: non-move ops ignore the destination entirely,
			// even when a caller stuffs an unauthorised one into the payload.
			name: "non-move op ignores a foreign destination",
			user: "alice", ws: "alice", proj: "notes", op: "write_page",
			destWS: strp("bob"), destProj: strp("notes"),
			wantAllow: true,
		},
		{
			name: "non-move op with no destination is unchanged",
			user: "alice", ws: "alice", proj: "notes", op: "delete",
			destWS: nil, destProj: nil,
			wantAllow: true,
		},
		{
			name: "non-move op still rejected on its own scope",
			user: "alice", ws: "bob", proj: "notes", op: "write_page",
			destWS: nil, destProj: nil,
			wantAllow: false, wantReason: "not allowed to write_page in 'bob'/'notes'",
		},
	}

	rules := mustCompile(t, acl)
	h := handler(discardLogger(), rules)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			allow, reason := evaluate(rules, tc.user, tc.ws, tc.proj, tc.op, tc.destWS, tc.destProj)
			if allow != tc.wantAllow {
				t.Fatalf("evaluate allow = %v, want %v (reason %q)", allow, tc.wantAllow, reason)
			}
			if tc.wantAllow && reason != "" {
				t.Fatalf("allowed decision must carry no reason, got %q", reason)
			}
			if !tc.wantAllow && !strings.Contains(reason, tc.wantReason) {
				t.Fatalf("reason %q must contain %q", reason, tc.wantReason)
			}

			// The same decision must come out of the HTTP surface, since the
			// destination fields have to survive JSON parsing to get there.
			code, body := postAdmit(t, h, payloadWithDest(tc.user, tc.ws, tc.proj, tc.op, tc.destWS, tc.destProj))
			wantCode := http.StatusForbidden
			if tc.wantAllow {
				wantCode = http.StatusOK
			}
			if code != wantCode {
				t.Fatalf("handler code = %d, want %d (body %s)", code, wantCode, body)
			}
			if tc.wantAllow {
				if body != "{}" {
					t.Fatalf("allow body must be empty JSON object, got %q", body)
				}
				return
			}
			var got map[string]string
			if err := json.Unmarshal([]byte(body), &got); err != nil {
				t.Fatalf("reject body must be JSON: %v (%s)", err, body)
			}
			if !strings.Contains(got["error"], tc.wantReason) {
				t.Fatalf("403 error %q must contain %q", got["error"], tc.wantReason)
			}
		})
	}
}

// A `null` destination is not something the engine emits (the field is skipped
// when None), but a hand-rolled payload can send it — it must land on the same
// fail-closed branch as an absent key rather than panicking on a nil deref.
func TestMoveWithNullDestinationIsFailClosed(t *testing.T) {
	rules := mustCompile(t, map[string][]ruleSpec{
		"alice": {{Workspace: ".*", Project: ".*"}},
	})
	h := handler(discardLogger(), rules)
	raw := `{"page":{"path":"x","frontmatter":{},"body":""},` +
		`"ctx":{"workspace":"alice","project":"notes","op":"move_project",` +
		`"destination_workspace":null,"destination_project":null,` +
		`"actor":{"user":"alice"}}}`
	req := httptest.NewRequest(http.MethodPost, "/admit", bytes.NewReader([]byte(raw)))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("null destination on a move must 403, got %d (%s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "destination_workspace=absent") {
		t.Fatalf("null must be reported like an absent key, got %s", rec.Body.String())
	}
}

// The ACL actually deployed today, exercised end to end.
func TestProductionACLBehaviour(t *testing.T) {
	rules := mustCompileJSON(t, prodACL)
	h := handler(discardLogger(), rules)

	cases := []struct {
		name      string
		user      string
		ws, proj  string
		op        string
		destWS    *string
		destProj  *string
		wantAllow bool
		wantSide  string
	}{
		{
			// The server's own scheduled work carries no actor by design and
			// is limited to consolidate/delete.
			name: "unattributed delete is admitted",
			user: "", ws: "djalmajr", proj: "ai-memory", op: "delete",
			wantAllow: true,
		},
		{
			name: "unattributed consolidate is admitted",
			user: "", ws: "djalmajr", proj: "ai-memory", op: "consolidate",
			wantAllow: true,
		},
		{
			// `ops` does not list move_project, so the scheduler is refused at
			// the source and never reaches the destination check.
			name: "unattributed move_project is rejected at the source",
			user: "", ws: "djalmajr", proj: "ai-memory", op: "move_project",
			destWS: strp("djalmajr"), destProj: strp("ai-memory"),
			wantAllow: false, wantSide: "(source scope)",
		},
		{
			name: "unattributed move_session is rejected at the source",
			user: "", ws: "default", proj: "development", op: "move_session",
			destWS: strp("default"), destProj: strp("notes"),
			wantAllow: false, wantSide: "(source scope)",
		},
		{
			name: "unattributed write_page stays rejected",
			user: "", ws: "djalmajr", proj: "ai-memory", op: "write_page",
			wantAllow: false, wantSide: "not allowed to write_page",
		},
		{
			// djalmajr's rule has no `ops` and matches every scope, so a
			// fully-specified move passes both ends.
			name: "djalmajr move across workspaces is admitted",
			user: "djalmajr", ws: "djalmajr", proj: "ai-memory", op: "move_project",
			destWS: strp("default"), destProj: strp("ai-memory"),
			wantAllow: true,
		},
		{
			// Even the unrestricted operator does not get a move with an
			// unknown destination: fail-closed is not a per-user policy.
			name: "djalmajr move without a destination is still rejected",
			user: "djalmajr", ws: "djalmajr", proj: "ai-memory", op: "move_project",
			wantAllow: false, wantSide: "move ops fail closed",
		},
		{
			name: "unknown user is rejected",
			user: "mallory", ws: "djalmajr", proj: "ai-memory", op: "write_page",
			wantAllow: false, wantSide: "not allowed to write_page",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, body := postAdmit(t, h, payloadWithDest(tc.user, tc.ws, tc.proj, tc.op, tc.destWS, tc.destProj))
			wantCode := http.StatusForbidden
			if tc.wantAllow {
				wantCode = http.StatusOK
			}
			if code != wantCode {
				t.Fatalf("code = %d, want %d (body %s)", code, wantCode, body)
			}
			if !tc.wantAllow && !strings.Contains(body, tc.wantSide) {
				t.Fatalf("403 body %s must contain %q", body, tc.wantSide)
			}
		})
	}
}
