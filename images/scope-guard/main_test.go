package main

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
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
