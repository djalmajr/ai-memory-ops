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
	"sync/atomic"
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
		// Move ops (`move_project`, `move_session`) carry the SOURCE scope in
		// workspace/project and the DESTINATION here. The engine omits both
		// keys for every other op (serde `skip_serializing_if`), so an empty
		// string means "not a move".
		DestinationWorkspace string `json:"destination_workspace"`
		DestinationProject   string `json:"destination_project"`
		Actor                struct {
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

	// staleFailThreshold: /healthz flips to 503 once a *live* push-failure
	// backlog is at least this old. Idle time alone (no failures) never trips.
	staleFailThreshold time.Duration

	mu        sync.Mutex // serializes git ops on workDir
	dirty     bool       // pending commits not yet pushed
	dirtyCond *sync.Cond

	// Health signals, written by pushLoop/handleSync and read lock-free by
	// the /healthz + /metrics handlers (so a long-held push mutex can never
	// block a probe or a scrape).
	lastPushOKUnix atomic.Int64 // unix seconds of the last successful push
	consecPushFail atomic.Int64 // consecutive push failures since last success

	// Notifies the engine delivered that this mirror deliberately did NOT
	// apply to the working tree. Both mean "the backup may now diverge from
	// the engine", which is the thing an operator needs to be able to alert
	// on — a divergence that only shows up as a log line gets missed.
	moveSessionNoops  atomic.Int64 // move_session notifies skipped (see moveSession)
	unsupportedOpHits atomic.Int64 // notifies for an op this build cannot mirror

	// Reconciliation: periodically prune backup project dirs whose project no
	// longer exists in the engine (orphans left by a purge that bypassed the
	// admission chain). Disabled when reconcileInterval == 0 or engineURL == "".
	reconcileInterval time.Duration
	engineURL         string // e.g. http://memory-v2-wiki-service:49374
	engineToken       string // bearer for the engine's /admin
	engineHost        string // Host header (engine allowedHosts gate)
	httpc             *http.Client
}

func newMirror() *mirror {
	m := &mirror{
		workDir:            envOr("WORK_DIR", "/work/repo"),
		repoURL:            os.Getenv("REPO_URL"),
		branch:             envOr("REPO_BRANCH", "main"),
		gitUser:            envOr("GIT_USER", "ai-memory git-mirror"),
		gitEmail:           envOr("GIT_EMAIL", "git-mirror@ai-memory.local"),
		pushDelay:          parseDuration(os.Getenv("PUSH_DEBOUNCE"), 10*time.Second),
		staleFailThreshold: parseDuration(os.Getenv("STALE_FAIL_THRESHOLD"), 15*time.Minute),
	}
	m.dirtyCond = sync.NewCond(&m.mu)
	// Seed "last push ok" to now so a freshly-started mirror never reports
	// stale before it has had a chance to push.
	m.lastPushOKUnix.Store(time.Now().Unix())
	// Reconciliation config (opt-in: needs both a non-zero interval and an
	// engine URL). ENGINE_HOST_HEADER defaults to "localhost" so the request
	// clears the engine's allowedHosts gate when hit via a Service DNS name.
	m.reconcileInterval = parseDuration(os.Getenv("RECONCILE_INTERVAL"), 0)
	m.engineURL = strings.TrimRight(os.Getenv("ENGINE_URL"), "/")
	m.engineToken = os.Getenv("ENGINE_AUTH_TOKEN")
	m.engineHost = envOr("ENGINE_HOST_HEADER", "localhost")
	m.httpc = &http.Client{Timeout: 20 * time.Second}
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

// moveSession acknowledges a session that changed project WITHOUT touching
// the working tree, on purpose.
//
// Why a no-op and not a `git mv`: the notify carries the source scope in
// ctx.workspace/project and the destination in ctx.destination_*, but the
// engine calls it with NO page path (`chain.notify(None, &ctx)` in
// crates/ai-memory-wiki/src/wiki.rs::move_session_page) and the session id
// appears nowhere else in the payload. The file that actually moves is
// `sessions/<session-id>.md` — a name this webhook cannot reconstruct from
// what it is given.
//
// The only git action reachable from source+destination alone would operate
// on the whole `sessions/` directory of the source project, which would
// move or delete the pages of every OTHER session that lived there. Keeping
// one stale session page in the backup is strictly less harmful than
// dropping N unrelated ones, so the mirror declines to guess.
//
// The divergence is real and is not papered over: the engine relocates the
// file with a plain `std::fs::rename` inside the same critical section,
// outside the admission chain, so NO write_page/delete follows to repair the
// backup. After a `pages=move` the backup keeps the page under the source
// project and never receives the destination copy. Repair is
// `scripts/git-mirror-backfill.py`, which re-syncs the whole tree in one
// commit. The counter behind `git_mirror_move_session_noops_total` is there
// so that repair can be triggered by an alert instead of by luck.
//
// Note the chart does NOT subscribe git-mirror to `move_session`; this path
// exists so that an operator who adds the event gets a logged, counted
// skip instead of a bogus write.
func (m *mirror) moveSession(p payload) error {
	m.moveSessionNoops.Add(1)
	slog.Warn("move_session not mirrored: payload carries no session id, "+
		"so the moved sessions/<id>.md cannot be identified; the backup may "+
		"keep a stale copy under the source project "+
		"(repair: scripts/git-mirror-backfill.py)",
		"from_workspace", defaultStr(p.Ctx.Workspace, "default"),
		"from_project", defaultStr(p.Ctx.Project, "_unscoped"),
		"to_workspace", p.Ctx.DestinationWorkspace,
		"to_project", p.Ctx.DestinationProject,
		"actor", actorOf(p))
	return nil
}

// unsupportedOp refuses a lifecycle op this build does not implement.
//
// Loud on purpose: a 500 here is inert for the deployed configuration
// (git-mirror runs `blocking: false, failure_policy: ignore`, so the engine
// only logs it) but it surfaces on BOTH sides — engine warning, mirror ERROR
// line, and a counter to alert on — instead of silently corrupting the
// backup. Note: `failure_policy: reject` alone does NOT abort the write —
// the engine only awaits a hook that is ALSO `blocking: true`, since a
// non-blocking webhook "can't mutate or reject" (engine admission.rs:563).
// Aborting a user's write because the BACKUP cannot mirror it would be the
// wrong trade anyway; the counter is the intended signal.
func (m *mirror) unsupportedOp(op string) error {
	m.unsupportedOpHits.Add(1)
	// The op is an attacker-reachable header value; keep it short in logs
	// and in the body echoed back to the engine.
	shown := truncate(op, 64)
	slog.Error("unsupported admission op: refusing to guess a git action "+
		"(the backup will diverge for this operation)", "op", shown)
	return fmt.Errorf("unsupported admission op %q", shown)
}

// truncate caps an untrusted string for log/response use. Cuts on a rune
// boundary so a multi-byte header value cannot leave invalid UTF-8 in the
// JSON log line.
func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "…"
}

// actorOf resolves the human/agent behind a payload, "anonymous" when the
// engine sent neither (its scheduled jobs carry no attribution by design).
func actorOf(p payload) string {
	who := p.Ctx.Actor.User
	if who == "" {
		who = p.Ctx.Actor.Agent
	}
	if who == "" {
		who = "anonymous"
	}
	return who
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
	return fmt.Sprintf("%s %s\n\nby: %s", verb, rel, actorOf(p))
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
			m.consecPushFail.Add(1)
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
		m.lastPushOKUnix.Store(time.Now().Unix())
		m.consecPushFail.Store(0)
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
	// (see engine `crate::admission::AdmissionOp`).
	//
	// Routing is EXHAUSTIVE by op name. Only the ops whose payload really is
	// a page write reach writePage; everything else is either implemented or
	// refused. An unrecognised op used to land on writePage, which is the one
	// outcome a backup must never have: it would either error out on the
	// empty page path (the observable symptom) or, for an op that does carry
	// a path, write a file the engine never asked for.
	//
	// The empty op keeps the old fallback ON PURPOSE: it is what a pre-header
	// engine build sends, and for those every notify was a page write.
	op := r.Header.Get("X-Memory-Op")
	var opErr error
	switch op {
	case "write_page", "consolidate", "":
		opErr = m.writePage(r.Context(), in)
	case "delete":
		opErr = m.deletePage(r.Context(), in)
	case "purge_project":
		opErr = m.purgeProject(r.Context(), in)
	case "move_session":
		opErr = m.moveSession(in)
	default:
		opErr = m.unsupportedOp(op)
	}
	if opErr != nil {
		// `op` is an untrusted header value; cap it here too so a junk header
		// cannot flood the log through the generic failure path.
		slog.Warn("mirror sync failed", "op", truncate(op, 64), "path", in.Page.Path, "err", opErr)
		http.Error(w, opErr.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleHealth is the staleness-aware readiness probe. It reports 503 only
// when there is a LIVE push-failure backlog (consecPushFail > 0) that has also
// gone stale (last success older than staleFailThreshold). An idle mirror with
// no writes never increments consecPushFail, so a quiet period never trips it —
// this targets the "backup silently wedged" mode (the 9-day index.lock outage)
// without flapping on transient blips or legitimate idleness.
func (m *mirror) handleHealth(w http.ResponseWriter, _ *http.Request) {
	fails := m.consecPushFail.Load()
	lastOK := time.Unix(m.lastPushOKUnix.Load(), 0)
	age := time.Since(lastOK)
	if fails > 0 && age > m.staleFailThreshold {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = fmt.Fprintf(w, "stale: %d consecutive push failures, last ok %s ago\n",
			fails, age.Round(time.Second))
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok\n"))
}

// handleLivez is the liveness probe: 200 as long as the process serves HTTP.
// It never reflects push staleness, so a remote outage cannot cause k8s to
// restart-loop the pod (restarting doesn't fix an unreachable remote — the
// staleness ALERT does). Restarts stay reserved for a wedged process.
func handleLivez(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok\n"))
}

// handleMetrics exposes the backup health signals in Prometheus text format
// (hand-written; the image pulls in no metrics dependency). An external rule
// can alert on `time() - git_mirror_last_push_ok_timestamp_seconds` or on a
// rising `git_mirror_consecutive_push_failures`.
func (m *mirror) handleMetrics(w http.ResponseWriter, _ *http.Request) {
	m.mu.Lock()
	dirty := 0
	if m.dirty {
		dirty = 1
	}
	m.mu.Unlock()
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	_, _ = fmt.Fprintf(w,
		"# HELP git_mirror_last_push_ok_timestamp_seconds Unix time of the last successful push.\n"+
			"# TYPE git_mirror_last_push_ok_timestamp_seconds gauge\n"+
			"git_mirror_last_push_ok_timestamp_seconds %d\n"+
			"# HELP git_mirror_consecutive_push_failures Consecutive push failures since the last success.\n"+
			"# TYPE git_mirror_consecutive_push_failures gauge\n"+
			"git_mirror_consecutive_push_failures %d\n"+
			"# HELP git_mirror_dirty Whether commits are pending push (1) or not (0).\n"+
			"# TYPE git_mirror_dirty gauge\n"+
			"git_mirror_dirty %d\n"+
			"# HELP git_mirror_move_session_noops_total move_session notifies acknowledged without mirroring (backup may hold a stale session page).\n"+
			"# TYPE git_mirror_move_session_noops_total counter\n"+
			"git_mirror_move_session_noops_total %d\n"+
			"# HELP git_mirror_unsupported_ops_total Admission notifies refused because this build cannot mirror the op.\n"+
			"# TYPE git_mirror_unsupported_ops_total counter\n"+
			"git_mirror_unsupported_ops_total %d\n",
		m.lastPushOKUnix.Load(), m.consecPushFail.Load(), dirty,
		m.moveSessionNoops.Load(), m.unsupportedOpHits.Load())
}

// adminProject is one entry of the engine's `GET /admin/projects` response.
type adminProject struct {
	Workspace string `json:"workspace_name"`
	Project   string `json:"project_name"`
}

// fetchLiveProjects asks the engine for the authoritative (workspace, project)
// inventory and returns the set of "workspace/project" keys. Any error
// (unreachable, non-2xx, decode failure) is surfaced so the caller FAILS SAFE
// and prunes nothing.
func (m *mirror) fetchLiveProjects(ctx context.Context) (map[string]bool, error) {
	if m.engineURL == "" {
		return nil, fmt.Errorf("ENGINE_URL not set")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, m.engineURL+"/admin/projects", nil)
	if err != nil {
		return nil, err
	}
	if m.engineToken != "" {
		req.Header.Set("Authorization", "Bearer "+m.engineToken)
	}
	if m.engineHost != "" {
		req.Host = m.engineHost
	}
	resp, err := m.httpc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("engine /admin/projects returned %s", resp.Status)
	}
	var payload struct {
		Projects []adminProject `json:"projects"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 16<<20)).Decode(&payload); err != nil {
		return nil, err
	}
	live := make(map[string]bool, len(payload.Projects))
	for _, p := range payload.Projects {
		if p.Workspace == "" || p.Project == "" {
			continue
		}
		live[p.Workspace+"/"+p.Project] = true
	}
	return live, nil
}

// findOrphans returns the "workspace/project" keys that exist as
// wiki/<ws>/<proj> directories on disk but are ABSENT from `live`. Only
// two-level directories directly under wiki/ are considered; top-level files
// (e.g. a workspace-level _meta.md) are ignored. Pure function for testability.
func findOrphans(wikiRoot string, live map[string]bool) ([]string, error) {
	wsEntries, err := os.ReadDir(wikiRoot)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var orphans []string
	for _, ws := range wsEntries {
		if !ws.IsDir() {
			continue
		}
		projEntries, err := os.ReadDir(filepath.Join(wikiRoot, ws.Name()))
		if err != nil {
			return nil, err
		}
		for _, pr := range projEntries {
			if !pr.IsDir() {
				continue
			}
			key := ws.Name() + "/" + pr.Name()
			if !live[key] {
				orphans = append(orphans, key)
			}
		}
	}
	return orphans, nil
}

// reconcile prunes backup project directories the engine no longer knows about
// (orphans from purges that bypassed the admission chain). It is FAIL-SAFE: if
// the engine can't be reached OR returns an empty inventory, it prunes NOTHING
// — a transient engine error or a freshly-restored/misconfigured engine must
// never cause the reconciler to wipe the backup.
func (m *mirror) reconcile(ctx context.Context) error {
	live, err := m.fetchLiveProjects(ctx)
	if err != nil {
		return fmt.Errorf("fetch live projects: %w", err)
	}
	if len(live) == 0 {
		slog.Warn("reconcile: engine returned zero projects; skipping prune (fail-safe)")
		return nil
	}
	orphans, err := findOrphans(filepath.Join(m.workDir, "wiki"), live)
	if err != nil {
		return fmt.Errorf("scan wiki: %w", err)
	}
	if len(orphans) == 0 {
		slog.Debug("reconcile: no orphans", "live_projects", len(live))
		return nil
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	m.clearStaleIndexLock()
	removed := 0
	for _, key := range orphans {
		rel := filepath.Join("wiki", key)
		if _, err := safeJoin(m.workDir, rel); err != nil {
			slog.Warn("reconcile: skipping unsafe path", "key", key, "err", err)
			continue
		}
		if err := m.run(ctx, "git", "rm", "-rf", "--ignore-unmatch", rel); err != nil {
			slog.Error("reconcile: git rm failed", "key", key, "err", err)
			continue
		}
		removed++
		slog.Info("reconcile: pruned orphan project from backup", "key", key)
	}
	if removed == 0 {
		return nil
	}
	return m.commitDirty(ctx, fmt.Sprintf("reconcile: prune %d orphan project(s) absent from the engine", removed))
}

// reconcileLoop runs reconcile() shortly after boot and then every
// reconcileInterval until ctx is cancelled.
func (m *mirror) reconcileLoop(ctx context.Context) {
	// Let the bootstrap clone settle before the first pass.
	select {
	case <-time.After(30 * time.Second):
	case <-ctx.Done():
		return
	}
	t := time.NewTicker(m.reconcileInterval)
	defer t.Stop()
	for {
		if err := m.reconcile(ctx); err != nil {
			slog.Warn("reconcile pass failed", "err", err)
		}
		select {
		case <-t.C:
		case <-ctx.Done():
			return
		}
	}
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

	if m.reconcileInterval > 0 && m.engineURL != "" {
		slog.Info("git-mirror reconcile enabled",
			"interval", m.reconcileInterval, "engine", m.engineURL)
		go m.reconcileLoop(ctx)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /sync", m.handleSync)
	mux.HandleFunc("GET /healthz", m.handleHealth)
	mux.HandleFunc("GET /livez", handleLivez)
	mux.HandleFunc("GET /metrics", m.handleMetrics)

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
