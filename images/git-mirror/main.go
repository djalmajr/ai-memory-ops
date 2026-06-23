// git-mirror — admission webhook for ai-memory.
//
// POST /sync receives { page, ctx } from the engine's admission chain and
// mirrors the page markdown into a local git working tree, then enqueues
// an asynchronous commit+push to a remote. The remote should be a private
// repo (e.g. gitlab.djalmajr.dev/djalmajr/ai-memory-bkp) used purely as
// the durable backup of the wiki.
//
// Wire (compatible with the engine's admission contract):
//
//	request: { page: { path, frontmatter, body }, ctx: { workspace, project, actor } }
//	204:     no-op (after enqueuing the write)
//
// The handler returns 204 unconditionally — the actual push is decoupled
// from the engine's write path so the user-visible latency never depends
// on the git remote being reachable.
//
// On startup the working dir is bootstrapped: if WORK_DIR/.git is absent,
// the configured REPO_URL is cloned (with the branch checked out). If the
// clone fails, the server still starts in degraded mode — writes are
// enqueued, the worker keeps retrying clone+push on each tick until the
// remote becomes reachable.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
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
		Workspace string `json:"workspace"`
		Project   string `json:"project"`
		Actor     struct {
			Agent string `json:"agent"`
			User  string `json:"user"`
		} `json:"actor"`
	} `json:"ctx"`
}

type mirror struct {
	workDir   string
	repoURL   string
	branch    string
	gitUser   string
	gitEmail  string
	pushDelay time.Duration

	mu        sync.Mutex // serializes git ops on workDir
	dirty     bool       // pending commits not yet pushed
	dirtyCond *sync.Cond
}

func newMirror() *mirror {
	m := &mirror{
		workDir:   envOr("WORK_DIR", "/work/repo"),
		repoURL:   os.Getenv("REPO_URL"),
		branch:    envOr("REPO_BRANCH", "main"),
		gitUser:   envOr("GIT_USER", "ai-memory git-mirror"),
		gitEmail:  envOr("GIT_EMAIL", "git-mirror@ai-memory.local"),
		pushDelay: parseDuration(os.Getenv("PUSH_DEBOUNCE"), 10*time.Second),
	}
	m.dirtyCond = sync.NewCond(&m.mu)
	return m
}

func envOr(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

func parseDuration(v string, d time.Duration) time.Duration {
	if v == "" {
		return d
	}
	if parsed, err := time.ParseDuration(v); err == nil {
		return parsed
	}
	return d
}

// bootstrap ensures the working tree exists and is on the configured
// branch. Idempotent — safe to call repeatedly. Returns nil only when
// the tree is ready for `git add`.
func (m *mirror) bootstrap(ctx context.Context) error {
	if m.repoURL == "" {
		return errors.New("REPO_URL is not set")
	}
	if _, err := os.Stat(filepath.Join(m.workDir, ".git")); err == nil {
		// Already cloned. Try to fast-forward but don't fail on it —
		// remote may be unreachable transiently.
		if err := m.run(ctx, "git", "fetch", "origin", m.branch); err != nil {
			slog.Warn("bootstrap fetch failed", "err", err)
		} else {
			_ = m.run(ctx, "git", "checkout", m.branch)
			_ = m.run(ctx, "git", "reset", "--hard", "origin/"+m.branch)
		}
		return nil
	}
	if err := os.MkdirAll(m.workDir, 0o755); err != nil {
		return fmt.Errorf("mkdir workdir: %w", err)
	}
	// Clone shallowly to keep the working set small — backup doesn't need
	// the full history of the remote.
	args := []string{"clone", "--branch", m.branch, "--depth", "1", m.repoURL, m.workDir}
	if err := m.runIn(ctx, "/", "git", args...); err != nil {
		// Cloning an empty repo with --branch fails with "Remote branch
		// not found". Fall back to plain clone + initial branch creation.
		slog.Warn("clone --branch failed, trying init", "err", err)
		if err := m.runIn(ctx, "/", "git", "clone", m.repoURL, m.workDir); err != nil {
			// Repo may be brand-new and empty — init locally and add the
			// origin so the first push creates the upstream branch.
			if err := os.RemoveAll(m.workDir); err != nil {
				return fmt.Errorf("cleanup workdir: %w", err)
			}
			if err := os.MkdirAll(m.workDir, 0o755); err != nil {
				return err
			}
			if err := m.run(ctx, "git", "init", "-b", m.branch); err != nil {
				return fmt.Errorf("init: %w", err)
			}
			if err := m.run(ctx, "git", "remote", "add", "origin", m.repoURL); err != nil {
				return fmt.Errorf("remote add: %w", err)
			}
		} else {
			_ = m.run(ctx, "git", "checkout", "-B", m.branch)
		}
	}
	if err := m.run(ctx, "git", "config", "user.name", m.gitUser); err != nil {
		return err
	}
	if err := m.run(ctx, "git", "config", "user.email", m.gitEmail); err != nil {
		return err
	}
	return nil
}

func (m *mirror) run(ctx context.Context, name string, args ...string) error {
	return m.runIn(ctx, m.workDir, name, args...)
}

// redactURL masks credentials embedded in a URL's userinfo so the backup
// remote's token (REPO_URL carries a "https://x-access-token:gho_...@github.com/..."
// PAT) never reaches the logs. The token surfaced in cleartext on the boot
// "git-mirror listening" line and inside clone errors. Non-URL strings and
// credential-free URLs pass through unchanged.
func redactURL(s string) string {
	i := strings.Index(s, "://")
	if i < 0 {
		return s
	}
	rest := s[i+3:]
	at := strings.IndexByte(rest, '@')
	if at < 0 {
		return s
	}
	// An '@' that comes after the first '/' is in the path, not userinfo.
	if slash := strings.IndexByte(rest, '/'); slash >= 0 && slash < at {
		return s
	}
	return s[:i+3] + "***@" + rest[at+1:]
}

// redactArgs returns a copy of args with any embedded URL credentials masked,
// for safe inclusion in log/error messages (clone/remote-add carry REPO_URL).
func redactArgs(args []string) []string {
	out := make([]string, len(args))
	for i, a := range args {
		out[i] = redactURL(a)
	}
	return out
}

func (m *mirror) runIn(ctx context.Context, dir, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	// Inherit env (SSH agent, GIT_SSH_COMMAND, http.https proxies, etc).
	cmd.Env = os.Environ()
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %s: %w: %s", name, strings.Join(redactArgs(args), " "), err, strings.TrimSpace(string(out)))
	}
	if len(out) > 0 {
		slog.Debug("cmd ok", "cmd", name, "args", redactArgs(args), "out", strings.TrimSpace(string(out)))
	}
	return nil
}

// clearStaleIndexLock removes a leftover .git/index.lock before an index op.
// Safe because every git op on workDir is serialised by m.mu, so any lock
// present while a caller holds m.mu is ORPHANED — left by a git subprocess
// killed mid-op (runIn uses exec.CommandContext, so a cancelled admission
// request kills `git add` and leaves the lock). Without this, the lock wedges
// EVERY subsequent sync and the backup stops in silence (observed: a 9-day
// gap, 2026-06). Callers must hold m.mu.
func (m *mirror) clearStaleIndexLock() {
	lock := filepath.Join(m.workDir, ".git", "index.lock")
	switch err := os.Remove(lock); {
	case err == nil:
		slog.Warn("removed stale git index.lock", "path", lock)
	case !errors.Is(err, os.ErrNotExist):
		slog.Warn("could not remove stale index.lock", "path", lock, "err", err)
	}
}

// writePage materializes a single page in the working tree and commits
// it. The push is handled by the background worker.
//
// On-disk layout in the backup repo:
//
//	wiki/<workspace>/<project>/<page-path>
//
// We re-serialise the page as `---\n<frontmatter yaml>\n---\n<body>` so
// the backup is a self-contained markdown file (matches the ai-memory
// on-disk layout's first frontmatter block).
func (m *mirror) writePage(ctx context.Context, p payload) error {
	if p.Page.Path == "" {
		return errors.New("page.path empty")
	}
	workspace := defaultStr(p.Ctx.Workspace, "default")
	project := defaultStr(p.Ctx.Project, "_unscoped")
	rel := filepath.Join("wiki", workspace, project, filepath.Clean(p.Page.Path))
	// Guard against absolute paths or `..` traversal in the engine input.
	full, err := safeJoin(m.workDir, rel)
	if err != nil {
		return err
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	m.clearStaleIndexLock()

	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return fmt.Errorf("mkdir: %w", err)
	}
	if err := os.WriteFile(full, renderPage(p), 0o644); err != nil {
		return fmt.Errorf("write: %w", err)
	}

	relPath, _ := filepath.Rel(m.workDir, full)
	if err := m.run(ctx, "git", "add", relPath); err != nil {
		return fmt.Errorf("git add: %w", err)
	}
	msg := commitMessage(p, relPath)
	if err := m.run(ctx, "git", "commit", "-m", msg, "--allow-empty"); err != nil {
		// Nothing to commit (identical content) is not an error path —
		// the engine may replay an unchanged page through the chain.
		if strings.Contains(err.Error(), "nothing to commit") {
			return nil
		}
		return fmt.Errorf("git commit: %w", err)
	}
	m.dirty = true
	m.dirtyCond.Signal()
	return nil
}

// deletePage removes a single page from the working tree and commits the
// removal. Mirrors the engine's `op=delete` admission notify (path only,
// no body). Idempotent: a path that isn't tracked is a no-op.
func (m *mirror) deletePage(ctx context.Context, p payload) error {
	if p.Page.Path == "" {
		return errors.New("page.path empty")
	}
	workspace := defaultStr(p.Ctx.Workspace, "default")
	project := defaultStr(p.Ctx.Project, "_unscoped")
	rel := filepath.Join("wiki", workspace, project, filepath.Clean(p.Page.Path))
	full, err := safeJoin(m.workDir, rel)
	if err != nil {
		return err
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	m.clearStaleIndexLock()

	relPath, _ := filepath.Rel(m.workDir, full)
	// `--ignore-unmatch` keeps the op idempotent when the engine replays a
	// delete for a page the mirror never had (e.g. created before the
	// mirror was enabled).
	if err := m.run(ctx, "git", "rm", "-f", "--ignore-unmatch", relPath); err != nil {
		return fmt.Errorf("git rm: %w", err)
	}
	return m.commitDirty(ctx, changeMessage("delete", p, relPath))
}

// purgeProject removes a whole project subtree from the working tree and
// commits it. Mirrors the engine's `op=purge_project` notify (no page path;
// the project comes from ctx). Idempotent.
func (m *mirror) purgeProject(ctx context.Context, p payload) error {
	workspace := defaultStr(p.Ctx.Workspace, "default")
	project := defaultStr(p.Ctx.Project, "_unscoped")
	rel := filepath.Join("wiki", workspace, project)
	full, err := safeJoin(m.workDir, rel)
	if err != nil {
		return err
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	m.clearStaleIndexLock()

	relPath, _ := filepath.Rel(m.workDir, full)
	if err := m.run(ctx, "git", "rm", "-rf", "--ignore-unmatch", relPath); err != nil {
		return fmt.Errorf("git rm -rf: %w", err)
	}
	return m.commitDirty(ctx, changeMessage("purge", p, relPath))
}

// commitDirty commits the currently-staged change and signals the push
// loop. A no-op staging area (engine replayed an already-applied removal)
// is treated as success. Callers hold m.mu.
func (m *mirror) commitDirty(ctx context.Context, msg string) error {
	if err := m.run(ctx, "git", "commit", "-m", msg); err != nil {
		if strings.Contains(err.Error(), "nothing to commit") {
			return nil
		}
		return fmt.Errorf("git commit: %w", err)
	}
	m.dirty = true
	m.dirtyCond.Signal()
	return nil
}

func defaultStr(v, d string) string {
	if v == "" {
		return d
	}
	return v
}

// safeJoin rejects path traversal: the resolved path must remain under base.
func safeJoin(base, rel string) (string, error) {
	full := filepath.Join(base, rel)
	abs, err := filepath.Abs(full)
	if err != nil {
		return "", err
	}
	baseAbs, err := filepath.Abs(base)
	if err != nil {
		return "", err
	}
	if !strings.HasPrefix(abs, baseAbs+string(os.PathSeparator)) && abs != baseAbs {
		return "", fmt.Errorf("path escapes base: %s", rel)
	}
	return abs, nil
}

func renderPage(p payload) []byte {
	var b strings.Builder
	// Serialise frontmatter as JSON inside a `---` block. The engine
	// itself uses YAML; we use JSON here to avoid pulling in a YAML lib.
	// The backup is a faithful structural copy, not a re-importable
	// markdown — restoration goes through `ai-memory restore` on a
	// tarball, not by re-importing this directory.
	b.WriteString("---\n")
	if p.Page.Frontmatter != nil {
		enc := json.NewEncoder(&b)
		enc.SetIndent("", "  ")
		_ = enc.Encode(p.Page.Frontmatter)
	}
	b.WriteString("---\n")
	b.WriteString(p.Page.Body)
	if !strings.HasSuffix(p.Page.Body, "\n") {
		b.WriteString("\n")
	}
	return []byte(b.String())
}

func commitMessage(p payload, rel string) string {
	return changeMessage("sync", p, rel)
}

// changeMessage builds a commit message "<verb> <rel>\n\nby: <actor>" for
// any working-tree change (sync/delete/purge).
func changeMessage(verb string, p payload, rel string) string {
	who := p.Ctx.Actor.User
	if who == "" {
		who = p.Ctx.Actor.Agent
	}
	if who == "" {
		who = "anonymous"
	}
	return fmt.Sprintf("%s %s\n\nby: %s", verb, rel, who)
}

// pushLoop runs forever, debouncing commits into batched pushes. It waits
// on dirtyCond for new commits, then sleeps `pushDelay` to let bursts
// coalesce before issuing one `git push`. A failed push leaves the
// dirty flag set so the next signal retries the whole pile.
func (m *mirror) pushLoop(ctx context.Context) {
	for {
		m.mu.Lock()
		for !m.dirty {
			if ctx.Err() != nil {
				m.mu.Unlock()
				return
			}
			m.dirtyCond.Wait()
		}
		m.mu.Unlock()

		// Debounce: let bursts coalesce before pushing.
		select {
		case <-time.After(m.pushDelay):
		case <-ctx.Done():
			return
		}

		m.mu.Lock()
		m.dirty = false
		// Push under the lock so a concurrent commit doesn't race the
		// `git push` reading refs/heads/<branch>.
		err := m.run(ctx, "git", "push", "-u", "origin", m.branch)
		// Self-heal: the remote may have moved ahead while we held our
		// clone (typical when someone runs the backfill script against
		// a fresh repo, or pushes via another tool). Rebase our local
		// commits on top of origin and try once more before giving up.
		// Without this, the loop spin-rejects forever (observed once
		// after delete+recreate of the GitHub repo).
		if err != nil && strings.Contains(err.Error(), "rejected") {
			if err2 := m.run(ctx, "git", "pull", "--rebase", "origin", m.branch); err2 == nil {
				err = m.run(ctx, "git", "push", "-u", "origin", m.branch)
			} else {
				slog.Warn("rebase before retry failed", "err", err2)
			}
		}
		m.mu.Unlock()
		if err != nil {
			slog.Error("push failed; will retry on next signal", "err", err)
			// Mark dirty again so the next write retriggers.
			m.mu.Lock()
			m.dirty = true
			m.mu.Unlock()
			// Cool-off before signalling ourselves to avoid hot-looping
			// against an unreachable remote.
			select {
			case <-time.After(30 * time.Second):
			case <-ctx.Done():
				return
			}
			m.mu.Lock()
			m.dirtyCond.Signal()
			m.mu.Unlock()
			continue
		}
		slog.Info("push ok", "branch", m.branch)
	}
}

func (m *mirror) handleSync(w http.ResponseWriter, r *http.Request) {
	defer r.Body.Close()
	body, err := io.ReadAll(io.LimitReader(r.Body, 8<<20))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	var in payload
	if err := json.Unmarshal(body, &in); err != nil {
		http.Error(w, "decode: "+err.Error(), http.StatusBadRequest)
		return
	}
	// Engine waits for our response synchronously inside admission. We
	// keep it tight: mutate working tree + commit, push is async.
	// failure_policy=ignore in the chart means even an error won't block
	// the engine. The lifecycle op rides in the `X-Memory-Op` header
	// (see engine `crate::admission::AdmissionOp`); an empty/unknown op
	// falls back to the write path for backward compatibility.
	op := r.Header.Get("X-Memory-Op")
	var opErr error
	switch op {
	case "delete":
		opErr = m.deletePage(r.Context(), in)
	case "purge_project":
		opErr = m.purgeProject(r.Context(), in)
	default:
		opErr = m.writePage(r.Context(), in)
	}
	if opErr != nil {
		slog.Warn("mirror sync failed", "op", op, "path", in.Page.Path, "err", opErr)
		http.Error(w, opErr.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func handleHealth(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok\n"))
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

func main() {
	setupLogger()
	addr := envOr("LISTEN_ADDR", "0.0.0.0:8080")

	m := newMirror()
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Bootstrap is best-effort: a failed clone leaves the server up so
	// the write queue can still buffer locally; the pushLoop's next pass
	// retries the clone path implicitly via `git push` against the
	// missing tree (which will fail until bootstrap eventually succeeds).
	if err := m.bootstrap(ctx); err != nil {
		slog.Error("bootstrap failed; running in degraded mode", "err", err)
	}

	go m.pushLoop(ctx)

	mux := http.NewServeMux()
	mux.HandleFunc("POST /sync", m.handleSync)
	mux.HandleFunc("GET /healthz", handleHealth)

	srv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	go func() {
		slog.Info("git-mirror listening", "addr", addr, "repo", redactURL(m.repoURL), "branch", m.branch)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("server error", "err", err)
			os.Exit(1)
		}
	}()

	<-ctx.Done()
	slog.Info("shutdown signal received")
	shutCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutCtx)
	// Wake the push loop so it can exit cleanly.
	m.mu.Lock()
	m.dirtyCond.Broadcast()
	m.mu.Unlock()
}
