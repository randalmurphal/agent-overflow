package git

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
)

const (
	// defaultTimeout bounds every git / gh / glab subprocess. It MUST
	// stay well below the frontend RPC timeout (RPC_TIMEOUT_MS in
	// frontend/src/lib/transport/wsClient.ts): when a forge CLI hangs
	// on a dead network, the descriptive error here ("gh ... timed out
	// after 45s") must reach the pane before the opaque client-side
	// "RPC timed out" fires.
	defaultTimeout        = 45 * time.Second
	defaultMaxOutputBytes = int64(1_000_000)

	// prLookupTTL is how long an open-PR lookup result stays cached.
	// PR state changes slowly (the user creates / closes / merges via
	// explicit actions), and our hot path (gitwatch refresh on every
	// fs-event-debounce) would otherwise shell `gh pr list` 4×/sec
	// during continuous file activity. A 30s ceiling caps that at ≤1
	// network round-trip per branch per 30s; explicit invalidation
	// after CreatePR keeps the freshly-opened PR visible immediately.
	prLookupTTL = 30 * time.Second
	// prLookupErrorTTL dampens watcher retry storms without making the user
	// wait after fixing auth/version problems; explicit refreshes invalidate
	// the cwd cache before re-checking.
	prLookupErrorTTL = 5 * time.Second
	// prStickyRetention is how long past its own expiry a cache entry is
	// kept so a failed lookup can still serve the last PR it saw (see
	// lookupOpenPR). It only widens the sweep horizon: the entry is never
	// served as a fresh answer past expiresAt, so this trades a few dozen
	// bytes per recently-visited branch for a badge that survives a `gh`
	// rate-limit or auth blip on a workspace that has been idle a while.
	prStickyRetention = 30 * time.Minute

	// FetchStaleWindow is how long after a successful `git fetch` the
	// app considers remote-tracking refs fresh enough to skip another
	// background fetch. The branch picker calls MaybeFetchRemotes on
	// open; staleness shorter than this returns quickly without
	// spawning git.
	FetchStaleWindow = 5 * time.Minute
)

// localeCEnv pins a child git's message catalogue to the C locale. See
// Core.runLocaleC for which invocations need it and why it is not applied
// to every subprocess.
var localeCEnv = []string{"LC_ALL=C", "LANG=C"}

// nonInteractiveEnv closes every channel a child git can use to ask a human
// for credentials. It is the DEFAULT for every subprocess this package
// spawns; the handful of user-initiated network commands opt back out via
// commandSpec.allowCredentialPrompt.
//
// Without it, a repository whose remote needs credentials turns the
// background cadence (gitwatch refresh, the branch picker's opportunistic
// `git fetch --all`) into a Git Credential Manager / ssh-askpass GUI dialog
// the user never asked for, or — with no askpass helper installed — a hang
// until our own 45s timeout fires.
//
// All five entries are load-bearing, verified against git 2.43.0 with a
// local HTTP endpoint answering 401:
//
//   - GIT_TERMINAL_PROMPT=0 alone is NOT enough. git's prompt chain tries
//     an askpass helper FIRST and only falls through to the terminal when
//     none is configured, so a user with GIT_ASKPASS (or core.askpass, or
//     SSH_ASKPASS) set still gets the dialog.
//   - GIT_ASKPASS= (empty, not unset) is what disables that chain. git
//     reads GIT_ASKPASS → core.askpass → SSH_ASKPASS in order and stops at
//     the first one that is *set*; an empty value therefore both fails the
//     `*askpass` non-empty test and shadows the two fallbacks. Unsetting it
//     instead would let core.askpass or SSH_ASKPASS take over.
//   - SSH_ASKPASS= / SSH_ASKPASS_REQUIRE=never cover the ssh:// transport,
//     where the prompt comes from ssh rather than from git.
//   - GCM_INTERACTIVE=never stops Git Credential Manager from opening its
//     own window before it ever reaches git's prompt chain.
//
// The result is a fast, descriptive failure ("could not read Username for
// ...: terminal prompts disabled") that reaches the pane as an error instead
// of a modal dialog behind the app window.
var nonInteractiveEnv = []string{
	"GIT_TERMINAL_PROMPT=0",
	"GIT_ASKPASS=",
	"SSH_ASKPASS=",
	"SSH_ASKPASS_REQUIRE=never",
	"GCM_INTERACTIVE=never",
}

type commandResult struct {
	stdout   string
	stderr   string
	exitCode int
}

// Worktree describes a git worktree attached to a repository.
type Worktree struct {
	Path   string `json:"path"`
	Branch string `json:"branch,omitempty"`
	HEAD   string `json:"head"`
}

// Core wraps git command execution with timeouts and bounded output capture.
type Core struct {
	timeout        time.Duration
	maxOutputBytes int64

	// prCache memoizes lookupOpenPR results so a refresh storm (gitwatch
	// firing every fs-event-debounce) doesn't translate into a `gh pr
	// list` storm. Keyed on (cwd, branch); cleared per-cwd after PR/MR
	// creation and explicit refresh paths. A process-global TTL'd cache is
	// the documented carve-out in internal/CLAUDE.md anti-patterns.
	//
	// RWMutex: the hot path (gitwatch refresh → lookupOpenPR) hits the
	// cache as a read most of the time; only cache misses + explicit
	// invalidation take the writer lock.
	prCacheMu sync.RWMutex
	prCache   map[string]prCacheEntry

	// forgeCache memoizes origin-URL classification per cwd. TTL'd via
	// forgeDetectionTTL (5 min); written by Core.Status each refresh
	// and Core.DetectForge on cache miss so both paths share warmup.
	// RWMutex for the same hot-read reason as prCacheMu.
	forgeCacheMu sync.RWMutex
	forgeCache   map[string]forgeCacheEntry

	// forges is the registered set of forge implementations indexed by
	// id ("github", "gitlab"). Populated in NewCore; not mutated after
	// construction so reads are lock-free.
	forges map[string]Forge

	// gitlabHosts is the current snapshot of self-hosted GitLab
	// hostnames (lowercase, deduped, validated) that classify as the
	// "gitlab" forge in addition to the literal gitlab.com match.
	// Replaced wholesale by SetGitLabHosts; reads take a copy under
	// the RWMutex so classification never races a settings update.
	gitlabHostsMu sync.RWMutex
	gitlabHosts   []string

	// untrackedLines memoizes per-file added-line counts for the
	// untracked-insertions badge, keyed cwd → rel path → (size, mtime,
	// lines). Written wholesale by every untrackedStats scan and swept
	// by untrackedCacheTTL, so it stays bounded by the workspaces
	// actively being watched. Plain Mutex: both scan phases (snapshot,
	// store) are single short critical sections with no hot read path
	// between them.
	untrackedMu    sync.Mutex
	untrackedLines map[string]*untrackedLineCache

	// gitDirCache memoizes successful resolveGitDir results per cwd. The
	// resolved git directory is immutable for a given cwd (a repo's .git
	// location never moves under it), and pendingOperation resolves it on
	// every gitwatch refresh edge — the memo drops one `git rev-parse
	// --git-dir` subprocess per refresh. Failures ("" — not a repo, or a
	// transient git error) are never cached so a later `git init` is seen.
	// Bounded by the number of distinct repo cwds touched in a session,
	// each entry two short path strings; RemoveWorktreeForce drops the
	// removed path's entry so a re-created worktree re-resolves.
	gitDirCacheMu sync.RWMutex
	gitDirCache   map[string]string

	// commonDirCache memoizes successful CommonDir results per cwd, for
	// the same reason (and with the same invalidation) as gitDirCache:
	// a repository's shared git directory never moves under a live path,
	// and the background-fetch cadence asks for it once per project per
	// tick. Failures are never cached.
	commonDirCacheMu sync.RWMutex
	commonDirCache   map[string]string

	// repoMetaMu guards the two repo-metadata probe caches below, both
	// keyed by canonical git common dir and TTL'd via repoMetaTTL (see
	// repo_meta_cache.go). They keep baseStatus's per-refresh
	// default-branch and origin-remote probes from spawning a subprocess
	// each on every gitwatch debounce edge.
	repoMetaMu         sync.RWMutex
	defaultBranchCache map[string]repoMetaEntry[string]
	originCache        map[string]repoMetaEntry[originIdentity]

	// fetchCache records the last time a `git fetch` succeeded against a
	// given repository, whoever ran it (MaybeFetchRemotes,
	// FetchRemotesBackground, PruneRemotes). Keyed by the CANONICAL GIT
	// COMMON DIR, not the repo root: every worktree of a repository
	// shares one common dir and one set of remote-tracking refs, so
	// keying on the root would let N worktrees of the same repo each
	// fetch it. RWMutex matches the other caches above.
	fetchCacheMu sync.RWMutex
	fetchCache   map[string]time.Time

	// fetchFlight collapses concurrent background fetches of the same
	// repository into one subprocess. Keyed like fetchCache.
	fetchFlight singleflight.Group

	// nowFn returns the current time. Production uses time.Now; tests
	// override to drive TTL expiry deterministically.
	nowFn func() time.Time

	// fetchFn runs the actual background `git fetch` for a repository.
	// Production wires fetchOriginQuiet; tests substitute it to count
	// and sequence invocations (single-flight has no observable effect
	// otherwise) without touching a network. Never reassigned in
	// production code.
	fetchFn func(ctx context.Context, cwd string) error
}

type prCacheEntry struct {
	url         string
	number      int
	lookupError string
	expiresAt   time.Time
	// origin is the origin-remote identity observed when url/number were
	// last read from the forge. A failed lookup carries the previous PR
	// forward only while this still matches (see lookupOpenPR), so a
	// workspace re-pointed at a different remote cannot keep showing a PR
	// that belongs to the old one. Unknown means "not observed" — never
	// "no remote" — and so never invalidates on its own.
	origin originIdentity
}

func prCacheKey(cwd, branch string) string {
	return cwd + "\x00" + branch
}

// NewCore returns a Core configured with the default timeout and output limit.
func NewCore() *Core {
	core := &Core{
		timeout:        defaultTimeout,
		maxOutputBytes: defaultMaxOutputBytes,
		prCache:        make(map[string]prCacheEntry),
		forgeCache:     make(map[string]forgeCacheEntry),
		gitDirCache:    make(map[string]string),
		commonDirCache: make(map[string]string),

		defaultBranchCache: make(map[string]repoMetaEntry[string]),
		originCache:        make(map[string]repoMetaEntry[originIdentity]),
		fetchCache:         make(map[string]time.Time),
		untrackedLines:     make(map[string]*untrackedLineCache),
		nowFn:              time.Now,
	}
	core.fetchFn = core.fetchOriginQuiet
	core.forges = map[string]Forge{
		"github": &githubForge{core: core},
		"gitlab": &gitlabForge{core: core},
	}
	return core
}

// forgeFor returns the Forge for cwd's detected origin remote. Returns
// nullForge when the origin is missing or the host is not a supported
// forge — every nullForge operation returns ErrUnsupportedForge so
// the caller can surface a single "not supported" message.
func (c *Core) forgeFor(cwd string) Forge {
	id := c.DetectForge(cwd)
	if id == "" {
		return nullForge{}
	}
	if f, ok := c.forges[id]; ok {
		return f
	}
	return nullForge{}
}

// ForgeByID returns the registered Forge with the given id, or
// nullForge if no such forge is registered. Used by callers that
// already know the forge id (e.g., a frontend-supplied "github" /
// "gitlab" string from a parsed PR/MR URL) so they can skip detection.
func (c *Core) ForgeByID(id string) Forge {
	if f, ok := c.forges[id]; ok {
		return f
	}
	return nullForge{}
}

// Execute runs git with the provided arguments. Non-zero exits are returned as errors.
func (c *Core) Execute(cwd string, args ...string) (stdout, stderr string, err error) {
	return c.executeSpec(commandSpec{binary: "git", cwd: cwd, args: args})
}

// executeLocaleC is Execute with git's messages pinned to the C locale.
// See runLocaleC for when that is required.
func (c *Core) executeLocaleC(cwd string, args ...string) (stdout, stderr string, err error) {
	return c.executeSpec(commandSpec{binary: "git", cwd: cwd, extraEnv: localeCEnv, args: args})
}

// executeInteractive is Execute for a network command the user just asked
// for and is waiting on, so a credential prompt is the right answer rather
// than a hard failure. See commandSpec.allowCredentialPrompt for the rule
// deciding which commands qualify.
func (c *Core) executeInteractive(cwd string, args ...string) (stdout, stderr string, err error) {
	return c.executeSpec(commandSpec{binary: "git", cwd: cwd, allowCredentialPrompt: true, args: args})
}

// runBinaryInteractive is runBinary for a forge-CLI command that shells out
// to `git push` on the user's behalf (`gh pr create`, `glab mr create`).
// The nested git inherits our environment, so the opt-out has to be made
// here rather than at a git call site we never see.
func (c *Core) runBinaryInteractive(binary, cwd string, args ...string) (commandResult, error) {
	return c.runSpec(commandSpec{binary: binary, cwd: cwd, allowCredentialPrompt: true, args: args})
}

func (c *Core) executeSpec(spec commandSpec) (stdout, stderr string, err error) {
	result, err := c.runSpec(spec)
	if err != nil {
		return "", "", err
	}
	if result.exitCode != 0 {
		return result.stdout, result.stderr, fmt.Errorf(
			"%s exited with code %d",
			formatCommand(spec.binary, spec.args...),
			result.exitCode,
		)
	}
	return result.stdout, result.stderr, nil
}

// CreateWorktree creates a new worktree backed by a new branch.
func (c *Core) CreateWorktree(cwd, path, branch string) error {
	return c.CreateWorktreeFromBranch(cwd, path, "", branch)
}

// AttachWorktree creates a new worktree at path pointing at an existing
// branch. Fails if the branch already has a worktree (git's own
// invariant — one branch checked out in one place at a time).
func (c *Core) AttachWorktree(cwd, path, branch string) error {
	if strings.TrimSpace(path) == "" {
		return errors.New("git worktree path is required")
	}
	branch = strings.TrimSpace(branch)
	if branch == "" {
		return errors.New("git worktree branch is required")
	}
	if err := validateBranchName(branch); err != nil {
		return err
	}
	_, stderr, err := c.Execute(cwd, "worktree", "add", "--", path, branch)
	if err != nil {
		message := strings.TrimSpace(stderr)
		if message == "" {
			message = err.Error()
		}
		return fmt.Errorf("git worktree add failed: %s", message)
	}
	return nil
}

// CreateWorktreeFromBranch creates a new worktree backed by newBranch from an
// explicit base branch, exactly as it exists locally. Empty baseBranch lets
// git use the current HEAD.
//
// Use this when the base is a ref only this machine has (a branch another
// worktree just cut). When the base is a branch that also lives on origin,
// CreateWorktreeFromFreshBase is the one to call: it refreshes origin first
// so the new worktree doesn't inherit a stale local base.
func (c *Core) CreateWorktreeFromBranch(cwd, path, baseBranch, newBranch string) error {
	return c.createWorktreeAt(cwd, path, baseBranch, newBranch, false)
}

// createWorktreeAt is the one `git worktree add -b` invocation. startPoint is
// a revision (a local branch, or a remote-tracking ref when the caller
// resolved one); empty means the repository's current HEAD.
//
// noTrack suppresses git's branch.autoSetupMerge DWIM, which would otherwise
// set the new branch's upstream whenever the start point is a remote-tracking
// ref. It is the caller's decision because it is only correct for a start
// point the caller resolved: a base the user named keeps git's default
// behaviour.
func (c *Core) createWorktreeAt(cwd, path, startPoint, newBranch string, noTrack bool) error {
	if strings.TrimSpace(path) == "" {
		return errors.New("git worktree path is required")
	}
	newBranch = strings.TrimSpace(newBranch)
	startPoint = strings.TrimSpace(startPoint)
	if newBranch == "" {
		return errors.New("git worktree branch is required")
	}
	if err := validateBranchName(newBranch); err != nil {
		return err
	}
	if startPoint != "" {
		if err := validateBranchName(startPoint); err != nil {
			return err
		}
	}
	if err := c.ensureBranchDoesNotExist(cwd, newBranch); err != nil {
		return err
	}

	args := []string{"worktree", "add"}
	if noTrack {
		args = append(args, "--no-track")
	}
	args = append(args, "-b", newBranch, path)
	if startPoint != "" {
		args = append(args, startPoint)
	}
	_, stderr, err := c.Execute(cwd, args...)
	if err != nil {
		if c.branchAlreadyExistsAfterCreationError(cwd, stderr, newBranch) {
			return branchAlreadyExistsError(newBranch)
		}
		message := strings.TrimSpace(stderr)
		if message == "" {
			message = err.Error()
		}
		return fmt.Errorf("git worktree add failed: %s", message)
	}
	return nil
}

// RemoveWorktree removes a worktree from a repository.
func (c *Core) RemoveWorktree(cwd, path string) error {
	return c.RemoveWorktreeForce(cwd, path, false)
}

// RemoveWorktreeForce removes a worktree from a repository, optionally using
// git's force flag for app-owned cleanup paths.
func (c *Core) RemoveWorktreeForce(cwd, path string, force bool) error {
	if strings.TrimSpace(path) == "" {
		return errors.New("git worktree path is required")
	}

	args := []string{"worktree", "remove"}
	if force {
		args = append(args, "--force")
	}
	// "--" so a flag-shaped path can never parse as a git option — this is
	// the one worktree argv that can carry a wire-supplied path.
	args = append(args, "--", path)
	_, stderr, err := c.Execute(cwd, args...)
	if err != nil {
		message := strings.TrimSpace(stderr)
		if message == "" {
			message = err.Error()
		}
		return fmt.Errorf("git worktree remove failed: %s", message)
	}
	// A worktree re-created at this path may get a different admin dir
	// under .git/worktrees/ — or belong to a different repository
	// entirely — so neither memoized directory may outlive it.
	c.gitDirCacheMu.Lock()
	delete(c.gitDirCache, path)
	c.gitDirCacheMu.Unlock()
	c.commonDirCacheMu.Lock()
	delete(c.commonDirCache, path)
	c.commonDirCacheMu.Unlock()
	return nil
}

// ListWorktrees returns all worktrees attached to the repository.
func (c *Core) ListWorktrees(cwd string) ([]Worktree, error) {
	result, err := c.run(cwd, "worktree", "list", "--porcelain")
	if err != nil {
		return nil, err
	}
	if result.exitCode != 0 {
		return nil, fmt.Errorf("git worktree list failed: %s", strings.TrimSpace(result.stderr))
	}
	return parseWorktreeList(result.stdout), nil
}

func (c *Core) run(cwd string, args ...string) (commandResult, error) {
	return c.runSpec(commandSpec{binary: "git", cwd: cwd, args: args})
}

// revParsePath resolves a directory-valued `git rev-parse` flag
// (--absolute-git-dir, --git-common-dir, …) to a cleaned absolute path.
// The bool is false — with a nil error — when cwd is not inside a
// repository, so callers can distinguish "no repo here" from "git
// failed". Relative output is resolved against cwd, which is what git
// itself means by it.
//
// Shared by watch-root discovery and CommonDir; keep it neutral about
// which one is calling.
func (c *Core) revParsePath(cwd string, arg string) (string, bool, error) {
	// runLocaleC: the non-repo branch below matches git's English message.
	result, err := c.runLocaleC(cwd, "rev-parse", arg)
	if err != nil {
		return "", false, fmt.Errorf("git rev-parse %s: %w", arg, err)
	}
	if result.exitCode != 0 {
		stderr := strings.TrimSpace(result.stderr)
		if strings.Contains(strings.ToLower(stderr), "not a git repository") {
			return "", false, nil
		}
		if stderr == "" {
			stderr = strings.TrimSpace(result.stdout)
		}
		return "", false, fmt.Errorf("git rev-parse %s failed: %s", arg, stderr)
	}

	path := strings.TrimSpace(result.stdout)
	if path == "" {
		return "", false, nil
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(cwd, path)
	}
	return filepath.Clean(path), true, nil
}

// runLocaleC runs git with its own messages pinned to the C locale. Use it
// for — and only for — invocations whose stdout/stderr this package
// pattern-matches in English: a git built with NLS translates "fatal: not a
// git repository", "No local changes to save", "CONFLICT (content):" and
// friends, so a non-English user would otherwise fall through the match
// into the wrong branch (a hard error instead of "not a repo", a phantom
// stash instead of "nothing to stash").
//
// Deliberately per-command rather than a blanket setting on the shared
// runner: LC_ALL also pins date, number, and collation formatting that
// other commands' output may legitimately want in the user's locale.
// LANG is set alongside it for belt and braces; LANGUAGE needs no handling
// because GNU gettext ignores it once the locale is C/POSIX.
func (c *Core) runLocaleC(cwd string, args ...string) (commandResult, error) {
	return c.runSpec(commandSpec{binary: "git", cwd: cwd, extraEnv: localeCEnv, args: args})
}

// runWithLimit runs a git command with an explicit stdout/stderr cap
// (0 = Core default). PR diffs can exceed the shared default, so their
// callers raise the ceiling rather than truncating.
func (c *Core) runWithLimit(cwd string, maxBytes int64, args ...string) (commandResult, error) {
	return c.runSpec(commandSpec{binary: "git", cwd: cwd, maxBytes: maxBytes, args: args})
}

func (c *Core) runBinary(binary, cwd string, args ...string) (commandResult, error) {
	return c.runSpec(commandSpec{binary: binary, cwd: cwd, args: args})
}

func (c *Core) runBinaryWithLimit(binary, cwd string, maxBytes int64, args ...string) (commandResult, error) {
	return c.runSpec(commandSpec{binary: binary, cwd: cwd, maxBytes: maxBytes, args: args})
}

func (c *Core) runBinaryInput(binary, cwd, stdin string, args ...string) (commandResult, error) {
	return c.runSpec(commandSpec{binary: binary, cwd: cwd, stdin: stdin, args: args})
}

// commandSpec fully describes one subprocess run. Every runner in this
// package funnels into runSpec with one of these, so a new dimension
// (extra env, stdin, a raised output cap) is expressed as a named field
// rather than as another positional overload of the low-level runner.
type commandSpec struct {
	// binary is the executable resolved on PATH: "git", "gh", or "glab".
	binary string
	// cwd is the child's working directory; empty inherits ours.
	cwd string
	// ctx, when non-nil, is the parent of the command's timeout context:
	// cancelling it kills the child. Set by callers whose work outlives
	// no one — the background fetch cadence hands in a context tied to
	// the app's lifetime so shutdown doesn't have to wait out a `git
	// fetch` hanging on a dead network. nil means "no owner", i.e. the
	// command is bounded only by the timeout.
	ctx context.Context
	// stdin is written to the child when non-empty.
	stdin string
	// maxBytes caps stdout and stderr independently; <= 0 falls back to
	// the Core's configured cap, then the package default.
	maxBytes int64
	// allowCredentialPrompt lets THIS command ask the user for credentials
	// (see nonInteractiveEnv, applied to every other command).
	//
	// The default is the safe one on purpose: a new background caller that
	// forgets to think about prompting gets a fast, descriptive failure,
	// while a new user-initiated network command that forgets this flag
	// fails loudly at the moment the user pressed the button. The inverse
	// default would make the dangerous case — a silent GUI dialog behind
	// the app window during the gitwatch cadence — the one you get by
	// omission.
	//
	// Set it ONLY for commands a human just asked for and is waiting on:
	// push, pull, explicit fetch/prune, and the forge CLIs' create paths
	// (which shell out to `git push` themselves). Never for anything on a
	// timer, a watcher edge, or a picker's opportunistic warm-up.
	allowCredentialPrompt bool
	// extraEnv is appended after the shared environment. os/exec keeps the
	// last occurrence of a duplicated key, so entries here override both
	// the inherited environment and the shared defaults below.
	extraEnv []string
	args     []string
}

// runSpec is the shared runner behind every git / gh / glab subprocess.
func (c *Core) runSpec(spec commandSpec) (commandResult, error) {
	timeout := c.timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	maxBytes := spec.maxBytes
	if maxBytes <= 0 {
		maxBytes = c.maxOutputBytes
	}
	if maxBytes <= 0 {
		maxBytes = defaultMaxOutputBytes
	}

	parent := spec.ctx
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, spec.binary, spec.args...)
	// Background-cadence git (`status` every debounce edge) must not
	// opportunistically rewrite .git/index: the write is a pure cache
	// optimization for git, but it fires an fs event under the watched
	// git dir — feeding the very refresh loop that ran the status, and
	// tripping the watcher's index-write rebuild trigger. Mandatory
	// locks (add, commit) are unaffected. Harmless for gh/glab.
	env := append(os.Environ(), "GIT_OPTIONAL_LOCKS=0")
	if !spec.allowCredentialPrompt {
		env = append(env, nonInteractiveEnv...)
	}
	cmd.Env = append(env, spec.extraEnv...)
	if spec.cwd != "" {
		cmd.Dir = spec.cwd
	}
	if spec.stdin != "" {
		cmd.Stdin = strings.NewReader(spec.stdin)
	}

	stdoutBuf := newLimitedBuffer(maxBytes)
	stderrBuf := newLimitedBuffer(maxBytes)
	cmd.Stdout = stdoutBuf
	cmd.Stderr = stderrBuf

	err := cmd.Run()
	result := commandResult{
		stdout: stdoutBuf.String(),
		stderr: stderrBuf.String(),
	}

	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return result, fmt.Errorf("%s timed out after %s", formatCommand(spec.binary, spec.args...), timeout)
	}
	// Cancelled by the caller's context rather than by our own timeout —
	// app shutdown killing a background command mid-flight. Distinct
	// message so it never reads as a repository or network problem.
	if errors.Is(ctx.Err(), context.Canceled) {
		return result, fmt.Errorf("%s cancelled", formatCommand(spec.binary, spec.args...))
	}
	if stdoutBuf.Truncated() || stderrBuf.Truncated() {
		return result, fmt.Errorf(
			"%s output exceeded %d bytes",
			formatCommand(spec.binary, spec.args...),
			maxBytes,
		)
	}
	if err == nil {
		return result, nil
	}

	if exitErr, ok := errors.AsType[*exec.ExitError](err); ok {
		result.exitCode = exitErr.ExitCode()
		return result, nil
	}

	return result, fmt.Errorf("%s failed: %w", formatCommand(spec.binary, spec.args...), err)
}

func formatCommand(binary string, args ...string) string {
	parts := make([]string, 0, 1+len(args))
	parts = append(parts, binary)
	for _, arg := range args {
		if strings.ContainsAny(arg, " \t\"'\\") {
			parts = append(parts, fmt.Sprintf("%q", arg))
		} else {
			parts = append(parts, arg)
		}
	}
	return strings.Join(parts, " ")
}

func parseWorktreeList(stdout string) []Worktree {
	var worktrees []Worktree
	current := Worktree{}

	flush := func() {
		if strings.TrimSpace(current.Path) == "" {
			return
		}
		worktrees = append(worktrees, current)
		current = Worktree{}
	}

	for _, rawLine := range strings.Split(stdout, "\n") {
		line := strings.TrimSpace(rawLine)
		if line == "" {
			flush()
			continue
		}

		switch {
		case strings.HasPrefix(line, "worktree "):
			flush()
			current.Path = strings.TrimSpace(strings.TrimPrefix(line, "worktree "))
		case strings.HasPrefix(line, "HEAD "):
			current.HEAD = strings.TrimSpace(strings.TrimPrefix(line, "HEAD "))
		case strings.HasPrefix(line, "branch "):
			current.Branch = trimBranchRef(strings.TrimSpace(strings.TrimPrefix(line, "branch ")))
		}
	}

	flush()
	return worktrees
}

func trimBranchRef(ref string) string {
	return strings.TrimPrefix(ref, "refs/heads/")
}

type limitedBuffer struct {
	buf       bytes.Buffer
	maxBytes  int64
	truncated bool
}

func newLimitedBuffer(maxBytes int64) *limitedBuffer {
	return &limitedBuffer{maxBytes: maxBytes}
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if b.maxBytes <= 0 {
		return len(p), nil
	}

	remaining := b.maxBytes - int64(b.buf.Len())
	if remaining <= 0 {
		b.truncated = true
		return len(p), nil
	}

	if int64(len(p)) > remaining {
		_, _ = b.buf.Write(p[:remaining])
		b.truncated = true
		return len(p), nil
	}

	_, _ = b.buf.Write(p)
	return len(p), nil
}

func (b *limitedBuffer) String() string {
	return b.buf.String()
}

func (b *limitedBuffer) Truncated() bool {
	return b.truncated
}
