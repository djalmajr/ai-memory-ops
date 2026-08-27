package main

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// e2e-ish: bootstrap an empty bare repo, run writePage, observe a commit
// land in the working tree. No remote push — just exercises the write/
// add/commit path.
func TestWritePageCommitsLocally(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir := t.TempDir()
	bare := filepath.Join(dir, "remote.git")
	work := filepath.Join(dir, "work")
	must(t, exec.Command("git", "init", "--bare", "-b", "main", bare).Run())

	t.Setenv("REPO_URL", bare)
	t.Setenv("WORK_DIR", work)
	t.Setenv("REPO_BRANCH", "main")
	t.Setenv("GIT_USER", "test")
	t.Setenv("GIT_EMAIL", "test@example.com")
	t.Setenv("PUSH_DEBOUNCE", "10ms")

	m := newMirror()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := m.bootstrap(ctx); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}

	p := decodePayload(t, `{
		"page": { "path": "gotchas/x.md", "frontmatter": {"title":"X"}, "body": "hello" },
		"ctx":  { "workspace": "default", "project": "wiki-service", "actor": { "user": "djalmajr" } }
	}`)
	if err := m.writePage(ctx, p); err != nil {
		t.Fatalf("writePage: %v", err)
	}

	target := filepath.Join(work, "wiki", "default", "wiki-service", "gotchas", "x.md")
	body, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read mirrored file: %v", err)
	}
	if !strings.Contains(string(body), `"title": "X"`) || !strings.Contains(string(body), "hello") {
		t.Fatalf("unexpected file content: %s", body)
	}

	// Verify a commit was actually recorded.
	out, err := exec.Command("git", "-C", work, "log", "--oneline").CombinedOutput()
	if err != nil {
		t.Fatalf("git log: %v: %s", err, out)
	}
	if !strings.Contains(string(out), "sync wiki/default/wiki-service/gotchas/x.md") {
		t.Fatalf("commit not found in log:\n%s", out)
	}
}

// A leftover .git/index.lock (from a git subprocess killed mid-op, e.g. a
// cancelled admission request) must not wedge the mirror: writePage heals it
// before staging. Without clearStaleIndexLock this fails with
// "Unable to create '.../index.lock': File exists" — the 9-day-silent-backup
// failure mode observed 2026-06.
func TestWritePageHealsStaleIndexLock(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir := t.TempDir()
	bare := filepath.Join(dir, "remote.git")
	work := filepath.Join(dir, "work")
	must(t, exec.Command("git", "init", "--bare", "-b", "main", bare).Run())

	t.Setenv("REPO_URL", bare)
	t.Setenv("WORK_DIR", work)
	t.Setenv("REPO_BRANCH", "main")
	t.Setenv("GIT_USER", "test")
	t.Setenv("GIT_EMAIL", "test@example.com")
	t.Setenv("PUSH_DEBOUNCE", "10ms")

	m := newMirror()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	must(t, m.bootstrap(ctx))

	// Simulate a crashed prior git op leaving an orphaned index.lock.
	lock := filepath.Join(work, ".git", "index.lock")
	must(t, os.WriteFile(lock, nil, 0o644))

	p := decodePayload(t, `{
		"page": { "path": "gotchas/x.md", "frontmatter": {"title":"X"}, "body": "hello" },
		"ctx":  { "workspace": "default", "project": "wiki-service", "actor": { "user": "djalmajr" } }
	}`)
	if err := m.writePage(ctx, p); err != nil {
		t.Fatalf("writePage with a stale index.lock should heal and succeed, got: %v", err)
	}

	if _, err := os.Stat(lock); !os.IsNotExist(err) {
		t.Fatalf("stale index.lock should have been removed, stat err=%v", err)
	}
	out, err := exec.Command("git", "-C", work, "log", "--oneline").CombinedOutput()
	if err != nil {
		t.Fatalf("git log: %v: %s", err, out)
	}
	if !strings.Contains(string(out), "sync wiki/default/wiki-service/gotchas/x.md") {
		t.Fatalf("commit not found in log:\n%s", out)
	}
}

func TestSafeJoinRejectsTraversal(t *testing.T) {
	base := t.TempDir()
	if _, err := safeJoin(base, "../etc/passwd"); err == nil {
		t.Fatal("expected traversal to be rejected")
	}
	if _, err := safeJoin(base, "ok/path.md"); err != nil {
		t.Fatalf("legit path rejected: %v", err)
	}
}

func TestRenderPageRoundtripsFrontmatter(t *testing.T) {
	p := decodePayload(t, `{
		"page": { "path": "x.md", "frontmatter": {"contributors": [{"agent":"a"}]}, "body": "body text" },
		"ctx":  {}
	}`)
	out := renderPage(p)
	if !strings.HasPrefix(string(out), "---\n") {
		t.Fatalf("missing frontmatter block: %s", out)
	}
	if !strings.Contains(string(out), `"contributors"`) {
		t.Fatalf("missing contributors key: %s", out)
	}
	if !strings.Contains(string(out), "body text\n") {
		t.Fatalf("missing/unterminated body: %s", out)
	}
}

func TestRedactURLMasksCredentials(t *testing.T) {
	cases := []struct{ in, want string }{
		// Token in userinfo — the real leak vector (REPO_URL with a gho_ PAT).
		{"https://x-access-token:gho_secret123@github.com/o/r.git", "https://***@github.com/o/r.git"},
		{"https://gho_secret123@github.com/o/r.git", "https://***@github.com/o/r.git"},
		// No credentials — passes through untouched.
		{"https://github.com/o/r.git", "https://github.com/o/r.git"},
		{"/work/repo", "/work/repo"},
		{"git@github.com:o/r.git", "git@github.com:o/r.git"},
		// An '@' in the path must not be mistaken for userinfo.
		{"https://github.com/o/r/@weird", "https://github.com/o/r/@weird"},
	}
	for _, c := range cases {
		if got := redactURL(c.in); got != c.want {
			t.Errorf("redactURL(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// bootstrapMirror spins up a bare remote + working clone and returns a
// ready mirror plus its working-tree path. Shared by the delete/purge tests.
func bootstrapMirror(t *testing.T) (*mirror, context.Context, string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir := t.TempDir()
	bare := filepath.Join(dir, "remote.git")
	work := filepath.Join(dir, "work")
	must(t, exec.Command("git", "init", "--bare", "-b", "main", bare).Run())

	t.Setenv("REPO_URL", bare)
	t.Setenv("WORK_DIR", work)
	t.Setenv("REPO_BRANCH", "main")
	t.Setenv("GIT_USER", "test")
	t.Setenv("GIT_EMAIL", "test@example.com")
	t.Setenv("PUSH_DEBOUNCE", "10ms")

	m := newMirror()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	if err := m.bootstrap(ctx); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	return m, ctx, work
}

func TestDeletePageRemovesFileAndCommits(t *testing.T) {
	m, ctx, work := bootstrapMirror(t)
	seed := decodePayload(t, `{
		"page": { "path": "notes/gone.md", "frontmatter": {}, "body": "bye" },
		"ctx":  { "workspace": "default", "project": "app", "actor": { "user": "djalmajr" } }
	}`)
	must(t, m.writePage(ctx, seed))

	target := filepath.Join(work, "wiki", "default", "app", "notes", "gone.md")
	if _, err := os.Stat(target); err != nil {
		t.Fatalf("seed file should exist: %v", err)
	}

	if err := m.deletePage(ctx, seed); err != nil {
		t.Fatalf("deletePage: %v", err)
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatalf("file should be gone after delete, stat err = %v", err)
	}
	out, err := exec.Command("git", "-C", work, "log", "--oneline").CombinedOutput()
	must(t, err)
	if !strings.Contains(string(out), "delete wiki/default/app/notes/gone.md") {
		t.Fatalf("delete commit not found in log:\n%s", out)
	}

	// Idempotent: a replayed delete on an already-gone path is a no-op.
	if err := m.deletePage(ctx, seed); err != nil {
		t.Fatalf("repeat deletePage should be a no-op: %v", err)
	}
}

func TestPurgeProjectRemovesSubtreeAndCommits(t *testing.T) {
	m, ctx, work := bootstrapMirror(t)
	for _, path := range []string{"a.md", "sub/b.md"} {
		must(t, m.writePage(ctx, decodePayload(t, `{
			"page": { "path": "`+path+`", "frontmatter": {}, "body": "x" },
			"ctx":  { "workspace": "default", "project": "doomed", "actor": { "user": "djalmajr" } }
		}`)))
	}
	projDir := filepath.Join(work, "wiki", "default", "doomed")
	if _, err := os.Stat(projDir); err != nil {
		t.Fatalf("project dir should exist: %v", err)
	}

	purge := decodePayload(t, `{
		"page": { "path": "", "frontmatter": {}, "body": "" },
		"ctx":  { "workspace": "default", "project": "doomed", "actor": { "user": "djalmajr" } }
	}`)
	if err := m.purgeProject(ctx, purge); err != nil {
		t.Fatalf("purgeProject: %v", err)
	}
	if _, err := os.Stat(projDir); !os.IsNotExist(err) {
		t.Fatalf("project dir should be gone after purge, stat err = %v", err)
	}
	out, err := exec.Command("git", "-C", work, "log", "--oneline").CombinedOutput()
	must(t, err)
	if !strings.Contains(string(out), "purge wiki/default/doomed") {
		t.Fatalf("purge commit not found in log:\n%s", out)
	}
}

func decodePayload(t *testing.T, raw string) payload {
	t.Helper()
	var p payload
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return p
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func getHandler(t *testing.T, h http.HandlerFunc) (int, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	h(rec, req)
	return rec.Code, rec.Body.String()
}

// The readiness probe trips ONLY on a live-and-stale push-failure backlog —
// never on a blip, and never on a legitimately idle mirror.
func TestHealthzStaleReturns503(t *testing.T) {
	m := newMirror()
	// Comfortably above the 1s truncation floor of the unix-seconds timestamp
	// (production uses 15m, so second-granularity is irrelevant there).
	m.staleFailThreshold = 10 * time.Second

	// Fresh: no failures → 200.
	if code, _ := getHandler(t, m.handleHealth); code != http.StatusOK {
		t.Fatalf("fresh mirror: want 200, got %d", code)
	}

	// Live failure backlog that has gone stale → 503.
	m.consecPushFail.Store(2)
	m.lastPushOKUnix.Store(time.Now().Add(-time.Hour).Unix())
	if code, body := getHandler(t, m.handleHealth); code != http.StatusServiceUnavailable {
		t.Fatalf("stale failing mirror: want 503, got %d (%s)", code, body)
	}

	// Idle but old (zero failures) → still 200: a quiet mirror must not trip.
	m.consecPushFail.Store(0)
	if code, _ := getHandler(t, m.handleHealth); code != http.StatusOK {
		t.Fatalf("idle-but-old mirror: want 200, got %d", code)
	}

	// Failing but within the threshold → 200 (transient-blip tolerance).
	m.consecPushFail.Store(1)
	m.lastPushOKUnix.Store(time.Now().Unix())
	if code, _ := getHandler(t, m.handleHealth); code != http.StatusOK {
		t.Fatalf("recent failure within threshold: want 200, got %d", code)
	}
}

// Liveness stays 200 through any staleness so a remote outage never triggers
// a k8s restart loop (which wouldn't fix an unreachable remote anyway).
func TestLivezAlwaysOKEvenWhenStale(t *testing.T) {
	m := newMirror()
	m.staleFailThreshold = time.Millisecond
	m.consecPushFail.Store(99)
	m.lastPushOKUnix.Store(time.Now().Add(-24 * time.Hour).Unix())
	if code, _ := getHandler(t, handleLivez); code != http.StatusOK {
		t.Fatalf("livez must be 200 regardless of staleness, got %d", code)
	}
}

func TestMetricsExposesHealthState(t *testing.T) {
	m := newMirror()
	m.lastPushOKUnix.Store(12345)
	m.consecPushFail.Store(3)
	m.mu.Lock()
	m.dirty = true
	m.mu.Unlock()
	m.moveSessionNoops.Store(2)
	m.unsupportedOpHits.Store(7)

	code, body := getHandler(t, m.handleMetrics)
	if code != http.StatusOK {
		t.Fatalf("metrics: want 200, got %d", code)
	}
	for _, want := range []string{
		"git_mirror_last_push_ok_timestamp_seconds 12345",
		"git_mirror_consecutive_push_failures 3",
		"git_mirror_dirty 1",
		"git_mirror_move_session_noops_total 2",
		"git_mirror_unsupported_ops_total 7",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics body missing %q\n---\n%s", want, body)
		}
	}
}

func TestFindOrphansIdentifiesDirsAbsentFromLive(t *testing.T) {
	root := t.TempDir()
	for _, p := range []string{"djalmajr/ai-memory", "djalmajr/grok-e2e-test", "default/scratch"} {
		if err := os.MkdirAll(filepath.Join(root, p, "decisions"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// A workspace-level file must be ignored, not treated as a project dir.
	if err := os.WriteFile(filepath.Join(root, "djalmajr", "_meta.md"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	live := map[string]bool{"djalmajr/ai-memory": true, "default/scratch": true}
	orphans, err := findOrphans(root, live)
	if err != nil {
		t.Fatal(err)
	}
	if len(orphans) != 1 || orphans[0] != "djalmajr/grok-e2e-test" {
		t.Fatalf("want [djalmajr/grok-e2e-test], got %v", orphans)
	}
}

func TestFindOrphansMissingRootIsNoError(t *testing.T) {
	orphans, err := findOrphans(filepath.Join(t.TempDir(), "nope"), map[string]bool{})
	if err != nil || orphans != nil {
		t.Fatalf("missing wiki root must be (nil,nil), got (%v,%v)", orphans, err)
	}
}

func TestFetchLiveProjectsParsesAndAuths(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/admin/projects" || r.Header.Get("Authorization") != "Bearer tok" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`{"projects":[{"workspace_name":"djalmajr","project_name":"ai-memory","page_count":3},{"workspace_name":"default","project_name":"scratch"}]}`))
	}))
	defer srv.Close()
	m := &mirror{engineURL: srv.URL, engineToken: "tok", httpc: srv.Client()}
	live, err := m.fetchLiveProjects(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(live) != 2 || !live["djalmajr/ai-memory"] || !live["default/scratch"] {
		t.Fatalf("unexpected live set: %v", live)
	}
}

func TestFetchLiveProjectsErrorsOnNon200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	m := &mirror{engineURL: srv.URL, httpc: srv.Client()}
	if _, err := m.fetchLiveProjects(context.Background()); err == nil {
		t.Fatal("want error on non-200 (fail-safe: caller prunes nothing)")
	}
}

// postSync drives the real HTTP handler, which is where op routing lives —
// calling the op methods directly would not catch a mis-wired switch.
// An empty `op` leaves the header off entirely (pre-header engine build).
func postSync(t *testing.T, m *mirror, op, body string) (int, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/sync", strings.NewReader(body))
	if op != "" {
		req.Header.Set("X-Memory-Op", op)
	}
	rec := httptest.NewRecorder()
	m.handleSync(rec, req)
	return rec.Code, rec.Body.String()
}

// commitCount counts commits on HEAD; a tree with no commits yet counts 0.
func commitCount(t *testing.T, work string) int {
	t.Helper()
	out, err := exec.Command("git", "-C", work, "rev-list", "--count", "HEAD").CombinedOutput()
	if err != nil {
		return 0
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil {
		t.Fatalf("rev-list --count: %q: %v", out, err)
	}
	return n
}

// The engine's move_session notify carries the source scope in ctx and the
// destination in ctx.destination_*. Locks the field names against the engine
// contract (crates/ai-memory-wiki/src/admission.rs: destination_workspace /
// destination_project) — a rename upstream must fail here, not silently log
// empty destinations in production.
func TestPayloadParsesDestinationScope(t *testing.T) {
	p := decodePayload(t, `{
		"page": { "path": "", "frontmatter": {}, "body": "" },
		"ctx":  {
			"workspace": "default", "project": "src",
			"destination_workspace": "djalmajr", "destination_project": "dst",
			"op": "move_session", "actor": { "user": "djalmajr" }
		}
	}`)
	if p.Ctx.Workspace != "default" || p.Ctx.Project != "src" {
		t.Fatalf("source scope not parsed: %+v", p.Ctx)
	}
	if p.Ctx.DestinationWorkspace != "djalmajr" || p.Ctx.DestinationProject != "dst" {
		t.Fatalf("destination scope not parsed: %+v", p.Ctx)
	}
	// Non-move ops omit the keys entirely (serde skip_serializing_if).
	q := decodePayload(t, `{
		"page": { "path": "a.md", "frontmatter": {}, "body": "x" },
		"ctx":  { "workspace": "default", "project": "src" }
	}`)
	if q.Ctx.DestinationWorkspace != "" || q.Ctx.DestinationProject != "" {
		t.Fatalf("absent destination keys must decode empty: %+v", q.Ctx)
	}
}

// move_session must NOT fall through to writePage (which errored on the empty
// page path) and must NOT guess a git action: the payload has no session id,
// so the only reachable target would be the whole sessions/ directory of the
// source project — taking every unrelated session's page with it. Contract:
// success to the engine, working tree untouched, counted for alerting.
func TestMoveSessionIsAcknowledgedWithoutTouchingTree(t *testing.T) {
	m, ctx, work := bootstrapMirror(t)
	for _, id := range []string{"019e-aaa", "019e-bbb"} {
		must(t, m.writePage(ctx, decodePayload(t, `{
			"page": { "path": "sessions/`+id+`.md", "frontmatter": {}, "body": "s" },
			"ctx":  { "workspace": "default", "project": "src", "actor": { "user": "djalmajr" } }
		}`)))
	}
	before := commitCount(t, work)

	code, body := postSync(t, m, "move_session", `{
		"page": { "path": "", "frontmatter": {}, "body": "" },
		"ctx":  {
			"workspace": "default", "project": "src",
			"destination_workspace": "default", "destination_project": "dst",
			"actor": { "user": "djalmajr" }
		}
	}`)
	if code != http.StatusNoContent {
		t.Fatalf("move_session: want 204, got %d (%s)", code, body)
	}

	// Every session page of the source project survives untouched.
	for _, id := range []string{"019e-aaa", "019e-bbb"} {
		if _, err := os.Stat(filepath.Join(work, "wiki", "default", "src", "sessions", id+".md")); err != nil {
			t.Fatalf("session page %s must survive a move_session: %v", id, err)
		}
	}
	// Nothing invented at the destination either.
	if _, err := os.Stat(filepath.Join(work, "wiki", "default", "dst")); !os.IsNotExist(err) {
		t.Fatalf("destination tree must not be fabricated, stat err = %v", err)
	}
	if got := commitCount(t, work); got != before {
		t.Fatalf("move_session must not commit: %d commits before, %d after", before, got)
	}
	if got := m.moveSessionNoops.Load(); got != 1 {
		t.Fatalf("move_session skip must be counted once, got %d", got)
	}
	if got := m.unsupportedOpHits.Load(); got != 0 {
		t.Fatalf("move_session is handled, not unsupported; got %d", got)
	}
}

// An op this build does not implement must fail loudly instead of being
// mirrored as a page write. The payload here is deliberately a VALID page
// write (non-empty path + body): under the old `default: writePage` routing
// it would have been committed happily under the wrong semantics, and the
// 500 would never have appeared.
func TestUnsupportedOpIsRefusedInsteadOfWritten(t *testing.T) {
	m, ctx, work := bootstrapMirror(t)
	must(t, m.writePage(ctx, decodePayload(t, `{
		"page": { "path": "seed.md", "frontmatter": {}, "body": "seed" },
		"ctx":  { "workspace": "default", "project": "app", "actor": { "user": "djalmajr" } }
	}`)))
	before := commitCount(t, work)

	for _, op := range []string{"move_project", "purge_workspace", "handoff_begin", "totally_new_op"} {
		code, body := postSync(t, m, op, `{
			"page": { "path": "notes/should-not-exist.md", "frontmatter": {}, "body": "nope" },
			"ctx":  { "workspace": "default", "project": "app", "actor": { "user": "djalmajr" } }
		}`)
		if code != http.StatusInternalServerError {
			t.Fatalf("op %q: want 500, got %d (%s)", op, code, body)
		}
		if !strings.Contains(body, "unsupported admission op") {
			t.Fatalf("op %q: error body should name the refusal, got %q", op, body)
		}
	}

	if _, err := os.Stat(filepath.Join(work, "wiki", "default", "app", "notes", "should-not-exist.md")); !os.IsNotExist(err) {
		t.Fatalf("a refused op must write nothing, stat err = %v", err)
	}
	if got := commitCount(t, work); got != before {
		t.Fatalf("a refused op must not commit: %d commits before, %d after", before, got)
	}
	if got := m.unsupportedOpHits.Load(); got != 4 {
		t.Fatalf("want 4 refusals counted, got %d", got)
	}
}

// A hostile/oversized op header is truncated everywhere it is echoed: the
// response body the engine logs, AND both of the mirror's own log lines.
func TestUnsupportedOpTruncatesHeaderEcho(t *testing.T) {
	m, _, _ := bootstrapMirror(t)

	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	long := strings.Repeat("A", 500)
	code, body := postSync(t, m, long, `{"page":{"path":"x.md"},"ctx":{}}`)
	if code != http.StatusInternalServerError {
		t.Fatalf("want 500, got %d", code)
	}
	if strings.Contains(body, long) || len(body) > 200 {
		t.Fatalf("op echo should be truncated, got %d bytes: %q", len(body), body)
	}
	if strings.Contains(logs.String(), long) {
		t.Fatalf("full op header leaked into the log:\n%s", logs.String())
	}
	if !strings.Contains(logs.String(), "unsupported admission op") {
		t.Fatalf("refusal should still be logged:\n%s", logs.String())
	}
}

// The ops that ARE page writes keep reaching writePage — including the empty
// header, the documented fallback for a pre-`X-Memory-Op` engine build.
func TestHandleSyncRoutesPageWriteOps(t *testing.T) {
	m, _, work := bootstrapMirror(t)
	for _, op := range []string{"write_page", "consolidate", ""} {
		name := op
		if name == "" {
			name = "no-header"
		}
		code, body := postSync(t, m, op, `{
			"page": { "path": "notes/`+name+`.md", "frontmatter": {"title":"T"}, "body": "b" },
			"ctx":  { "workspace": "default", "project": "app", "actor": { "user": "djalmajr" } }
		}`)
		if code != http.StatusNoContent {
			t.Fatalf("op %q: want 204, got %d (%s)", op, code, body)
		}
		if _, err := os.Stat(filepath.Join(work, "wiki", "default", "app", "notes", name+".md")); err != nil {
			t.Fatalf("op %q should have mirrored the page: %v", op, err)
		}
	}
	// ...and delete still removes, through the same handler.
	code, body := postSync(t, m, "delete", `{
		"page": { "path": "notes/write_page.md", "frontmatter": {}, "body": "" },
		"ctx":  { "workspace": "default", "project": "app", "actor": { "user": "djalmajr" } }
	}`)
	if code != http.StatusNoContent {
		t.Fatalf("delete: want 204, got %d (%s)", code, body)
	}
	if _, err := os.Stat(filepath.Join(work, "wiki", "default", "app", "notes", "write_page.md")); !os.IsNotExist(err) {
		t.Fatalf("delete should have removed the page, stat err = %v", err)
	}
	if got := m.unsupportedOpHits.Load(); got != 0 {
		t.Fatalf("known ops must not be counted as unsupported, got %d", got)
	}
}
