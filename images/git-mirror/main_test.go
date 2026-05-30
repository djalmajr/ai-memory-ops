package main

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
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
