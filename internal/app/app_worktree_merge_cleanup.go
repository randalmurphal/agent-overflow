package app

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"agent-overflow/internal/forgeapi"
	gitops "agent-overflow/internal/git"
	"agent-overflow/internal/prthread"
	"agent-overflow/internal/store"
)

// Merged pull request worktree cleanup (docs/decisions.md, "A thread's
// worktree is removed when its pull request merges").
//
// The app records the pull request a thread in a linked worktree was seen
// with OPEN (thread_pull_requests): every fresh answer of the open-PR
// lookup reaches observeOpenPR, whoever asked (a watched workspace's git
// status, GetGitStatus, the refresh an agent's `gh pr create` prompts).
// Only a pull request seen open counts, so an old merged or closed one on
// a reused branch name is never adopted.
//
// A merge is the forge's word for it, read two ways. When the lookup for a
// tracked branch answers "no open PR", the tracked pull request's state is
// read once. And every pollInterval each repository with an unmerged
// tracked pull request gets one batched state read (git.ReadPRStates).
// Neither compares commits, so a squash, rebase or force push changes
// nothing; a pull request closed without merging is retired.
//
// A merged pull request is acted on once, when every thread in its
// worktree is idle: nothing happens when the thread left the worktree, the
// user wrote in a thread there after the merge, or the branch has a new
// open pull request; uncommitted changes keep the worktree with one notice;
// otherwise the worktree is removed through the in-app removal, keeping
// the branch, and each thread that moved to Base is told. A removal git
// refuses is told once and retried. A failure to read the store or the
// forge waits for a later pass; it never decides an outcome.

const (
	// mergedPRPollInterval is how often one repository's unmerged tracked
	// pull requests are read, the background fetch's cadence.
	mergedPRPollInterval = 5 * time.Minute
	// mergedPRPollTick is how often the worker looks for a repository that
	// is due, and for a merged pull request whose threads went idle.
	mergedPRPollTick = time.Minute
	// mergedPRPollInitialDelay holds the first poll after boot, as the
	// background fetch does.
	mergedPRPollInitialDelay = 45 * time.Second
	// mergedPRPollBackoffMax caps the wait after consecutive failed reads
	// of one repository.
	mergedPRPollBackoffMax = 30 * time.Minute
	// mergedPRForgeTimeout bounds one forge read the cleanup makes.
	mergedPRForgeTimeout = time.Minute
)

// mergedPRBackoff paces one kind of forge request or removal attempt for
// one subject: due when next has passed, doubling its wait per
// consecutive failure.
type mergedPRBackoff struct {
	next     time.Time
	failures int
}

func (b *mergedPRBackoff) due(now time.Time) bool { return b == nil || !now.Before(b.next) }

// fail schedules the next attempt base<<failures after now, at most
// mergedPRPollBackoffMax (or base, when that is longer), or at the time a
// rate limit lifts.
func (b *mergedPRBackoff) fail(now time.Time, base time.Duration, err error) {
	b.failures++
	if limited, ok := errors.AsType[*forgeapi.RateLimitedError](err); ok && limited.Until.After(now) {
		b.next = limited.Until
		return
	}
	b.next = now.Add(min(base<<min(b.failures, 8), max(mergedPRPollBackoffMax, base)))
}

// mergedPRCleanupState is the cleanup's worker. observed and act are
// handed over under mu; reads, lookups, removals, noneRead and logged
// belong to the worker goroutine.
type mergedPRCleanupState struct {
	// interval replaces mergedPRPollInterval for a harness boot
	// (IsolationConfig.MergedPRPollInterval); zero keeps it.
	interval time.Duration

	mu       sync.Mutex
	observed map[string]gitops.OpenPRObservation
	act      bool
	wake     chan struct{}
	stop     chan struct{}
	done     chan struct{}

	// waitingIdle is set while a merged pull request waits for its
	// worktree's threads to go idle, so a turn's end wakes the worker.
	waitingIdle atomic.Bool

	// reads paces each repository's batched state read; lookups each
	// repository's open-PR lookup before a removal; removals each merged
	// pull request's retried removal.
	reads    map[string]*mergedPRBackoff
	lookups  map[string]*mergedPRBackoff
	removals map[string]*mergedPRBackoff
	// noneRead is, per thread, the tracked pull request already read once
	// because a lookup stopped finding it. Pruned to live tracked rows.
	noneRead map[string]string
	logged   map[string]string
}

func (c *mergedPRCleanupState) pollInterval() time.Duration {
	if c.interval > 0 {
		return c.interval
	}
	return mergedPRPollInterval
}

// tick is how often the worker wakes on its own, and the first wait of a
// failed lookup or removal.
func (c *mergedPRCleanupState) tick() time.Duration {
	return min(mergedPRPollTick, c.pollInterval())
}

// backoffFor is key's entry in m, created on first use.
func backoffFor(m *map[string]*mergedPRBackoff, key string) *mergedPRBackoff {
	if *m == nil {
		*m = make(map[string]*mergedPRBackoff)
	}
	b := (*m)[key]
	if b == nil {
		b = &mergedPRBackoff{}
		(*m)[key] = b
	}
	return b
}

// wakeLocked is the worker's wake channel. Callers hold mu.
func (c *mergedPRCleanupState) wakeLocked() chan struct{} {
	if c.wake == nil {
		c.wake = make(chan struct{}, 1)
	}
	return c.wake
}

func (c *mergedPRCleanupState) signalLocked() {
	select {
	case c.wakeLocked() <- struct{}{}:
	default:
	}
}

// observeOpenPR is the git core's open-PR observer. It runs inside a git
// status refresh, so it only hands the answer to the worker: the latest
// answer per checkout and branch, bounded by the checkouts being looked up.
func (a *App) observeOpenPR(obs gitops.OpenPRObservation) {
	c := &a.mergedPRs
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.observed == nil {
		c.observed = make(map[string]gitops.OpenPRObservation)
	}
	c.observed[gitops.CanonicalPath(obs.Cwd)+"\x00"+obs.Branch] = obs
	c.signalLocked()
}

// wakeMergedPRCleanup asks the worker for an act pass when a merged pull
// request is waiting for its threads to go idle. Called when a turn ends.
func (a *App) wakeMergedPRCleanup() {
	c := &a.mergedPRs
	if !c.waitingIdle.Load() {
		return
	}
	c.mu.Lock()
	c.act = true
	c.signalLocked()
	c.mu.Unlock()
}

// startMergedPRCleanup starts the worker. It waits for the first client's
// catalog reads like every unattended scan, then acts on merged pull
// requests a previous run left waiting, and polls from the initial delay.
func (a *App) startMergedPRCleanup() {
	c := &a.mergedPRs
	c.mu.Lock()
	if c.stop != nil {
		c.mu.Unlock()
		return
	}
	stop, done := make(chan struct{}), make(chan struct{})
	c.stop, c.done = stop, done
	wake := c.wakeLocked()
	c.mu.Unlock()

	go func() {
		defer close(done)
		ctx, cancel := context.WithCancel(a.lifeCtx())
		defer cancel()
		go func() {
			select {
			case <-stop:
				cancel()
			case <-ctx.Done():
			}
		}()
		if err := a.awaitFirstReadsSettled(ctx); err != nil {
			return
		}
		interval := c.pollInterval()
		pollFrom := time.Now().Add(min(mergedPRPollInitialDelay, interval))
		ticker := time.NewTicker(min(mergedPRPollTick, interval))
		defer ticker.Stop()
		a.mergedPRActPass(ctx)
		for {
			select {
			case <-ctx.Done():
				return
			case <-wake:
				a.mergedPRDrain(ctx)
			case now := <-ticker.C:
				a.mergedPRDrain(ctx)
				if !now.Before(pollFrom) {
					a.mergedPRPoll(ctx, now)
				}
				a.mergedPRActPass(ctx)
			}
		}
	}()
}

// stopMergedPRCleanup stops the worker and joins it. It removes worktrees
// and stops sessions, so Shutdown joins it before the session close and the
// store close. Safe before start and twice.
func (a *App) stopMergedPRCleanup() {
	c := &a.mergedPRs
	c.mu.Lock()
	stop, done := c.stop, c.done
	c.stop = nil
	c.mu.Unlock()
	if stop == nil {
		return
	}
	close(stop)
	<-done
}

// mergedPRDrain handles the observations handed over since the last drain,
// then runs an act pass when one was asked for or a merge was found.
func (a *App) mergedPRDrain(ctx context.Context) {
	c := &a.mergedPRs
	c.mu.Lock()
	observed := c.observed
	c.observed = nil
	act := c.act
	c.act = false
	c.mu.Unlock()
	keys := make([]string, 0, len(observed))
	for key := range observed {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	for _, key := range keys {
		merged, err := a.handleOpenPRObservation(ctx, observed[key])
		if merged {
			act = true
		}
		if err != nil {
			// Handled again at the next tick, unless a newer answer for the
			// same checkout arrived meanwhile.
			a.logMergedPR("observe:"+key, fmt.Sprintf("merged PR cleanup: %v", err))
			c.mu.Lock()
			if c.observed == nil {
				c.observed = make(map[string]gitops.OpenPRObservation)
			}
			if _, newer := c.observed[key]; !newer {
				c.observed[key] = observed[key]
			}
			c.mu.Unlock()
			continue
		}
		a.clearMergedPRLog("observe:" + key)
	}
	if act {
		a.mergedPRActPass(ctx)
	}
}

// linkedWorktreeThreadsAt returns the threads running in path as their
// linked worktree. A thread in its project root is never one. An error
// means some thread could not be judged; the caller tries again later.
func (a *App) linkedWorktreeThreadsAt(path string) ([]store.Thread, error) {
	ids, err := a.threadsReferencingWorkspace(path)
	if err != nil {
		return nil, fmt.Errorf("threads in %s: %w", path, err)
	}
	slices.Sort(ids)
	ids = slices.Compact(ids)
	threads := make([]store.Thread, 0, len(ids))
	for _, id := range ids {
		t, err := a.store.GetThread(id)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return nil, err
		}
		linked, err := a.threadInLinkedWorktree(t, path)
		if err != nil {
			return nil, err
		}
		if linked {
			threads = append(threads, t)
		}
	}
	return threads, nil
}

// threadInLinkedWorktree reports whether t runs in path as a linked
// worktree of its project. It fails, rather than answering no, when t's
// project root is unknown, since path could be that root.
func (a *App) threadInLinkedWorktree(t store.Thread, path string) (bool, error) {
	if !gitops.SameFilesystemPath(t.WorktreePath, path) || !gitops.SameFilesystemPath(t.WorkspacePath, path) {
		return false, nil
	}
	project := strings.TrimSpace(t.ProjectPath)
	if project == "" {
		return false, fmt.Errorf("thread %s has no project root", t.ID)
	}
	return !gitops.SameFilesystemPath(project, path), nil
}

// handleOpenPRObservation records an open pull request for every thread in
// the observed checkout, or, when the checkout's branch has none, reads the
// state of the pull request its threads track once per disappearance.
// Reports whether a merge was recorded, and an error when the observation
// should be handled again.
func (a *App) handleOpenPRObservation(ctx context.Context, obs gitops.OpenPRObservation) (bool, error) {
	threads, err := a.linkedWorktreeThreadsAt(obs.Cwd)
	if err != nil || len(threads) == 0 {
		return false, err
	}
	c := &a.mergedPRs
	if obs.PR != nil {
		var errs []error
		for _, t := range threads {
			if err := a.recordOpenPR(t.ID, obs.Cwd, obs.Branch, *obs.PR); err != nil {
				errs = append(errs, err)
			}
			delete(c.noneRead, t.ID)
		}
		return false, errors.Join(errs...)
	}
	var due []store.ThreadPullRequest
	for _, t := range threads {
		row, ok, err := a.store.GetThreadPullRequest(t.ID)
		if err != nil {
			return false, err
		}
		if !ok || row.HandledAt != 0 || row.MergedAt != 0 || row.Branch != obs.Branch || !gitops.SameFilesystemPath(row.WorktreePath, obs.Cwd) {
			continue
		}
		if c.noneRead[t.ID] == threadPullRequestKey(row) {
			continue
		}
		due = append(due, row)
	}
	merged := false
	for _, group := range groupByRepository(due) {
		states, err := a.readPRStates(ctx, group)
		if err != nil {
			// The repository's poll reads it later; this disappearance
			// does not ask again.
			a.logMergedPR("read:"+repositoryKey(group[0]), fmt.Sprintf("merged PR cleanup: read %s: %v", group[0].URL, err))
			continue
		}
		a.clearMergedPRLog("read:" + repositoryKey(group[0]))
		if c.noneRead == nil {
			c.noneRead = make(map[string]string)
		}
		for _, row := range group {
			c.noneRead[row.ThreadID] = threadPullRequestKey(row)
		}
		if a.applyPRStates(group, states) {
			merged = true
		}
	}
	return merged, nil
}

// pruneNoneRead drops the entries of threads whose tracked pull request is
// no longer the pending one the entry names: deleted threads, threads that
// left their worktree, handled or replaced pull requests. pending is every
// unhandled row.
func (a *App) pruneNoneRead(pending []store.ThreadPullRequest) {
	c := &a.mergedPRs
	if len(c.noneRead) == 0 {
		return
	}
	live := make(map[string]store.ThreadPullRequest, len(pending))
	for _, row := range pending {
		live[row.ThreadID] = row
	}
	for threadID, key := range c.noneRead {
		row, ok := live[threadID]
		if !ok || threadPullRequestKey(row) != key {
			delete(c.noneRead, threadID)
			continue
		}
		t, err := a.store.GetThread(threadID)
		if errors.Is(err, sql.ErrNoRows) {
			delete(c.noneRead, threadID)
			continue
		}
		if err != nil {
			continue
		}
		if linked, err := a.threadInLinkedWorktree(t, row.WorktreePath); err == nil && !linked {
			delete(c.noneRead, threadID)
		}
	}
}

// recordOpenPR tracks pr as the open pull request of threadID's worktree.
// A pull request the app cannot track is logged and is not an error; a
// store failure is, so the observation is handled again.
func (a *App) recordOpenPR(threadID, worktree, branch string, pr gitops.GitPR) error {
	ref, err := gitops.ParsePRURL(pr.URL)
	if err != nil {
		a.logMergedPR("url:"+pr.URL, fmt.Sprintf("merged PR cleanup: open PR %q: %v", pr.URL, err))
		return nil
	}
	if ref.Forge == "github" && pr.ID == "" {
		a.logMergedPR("node:"+pr.URL, fmt.Sprintf("merged PR cleanup: GitHub reported no node id for %s", pr.URL))
		return nil
	}
	if _, err := a.store.RecordOpenThreadPullRequest(store.ThreadPullRequest{
		ThreadID: threadID, Forge: ref.Forge, Host: ref.Host, Project: ref.Project(), Number: ref.Number,
		URL: pr.URL, NodeID: pr.ID, Branch: branch, WorktreePath: worktree, SeenOpenAt: time.Now().UnixMilli(),
	}); err != nil {
		return fmt.Errorf("record open PR of thread %s: %w", threadID, err)
	}
	return nil
}

func threadPullRequestKey(row store.ThreadPullRequest) string {
	return fmt.Sprintf("%s\x00%s\x00%s\x00%d", row.Forge, row.Host, row.Project, row.Number)
}

func repositoryKey(row store.ThreadPullRequest) string {
	return row.Forge + "\x00" + row.Host + "\x00" + row.Project
}

// groupByRepository splits rows by the repository of their pull request,
// in first-seen order.
func groupByRepository(rows []store.ThreadPullRequest) [][]store.ThreadPullRequest {
	index := make(map[string]int)
	var groups [][]store.ThreadPullRequest
	for _, row := range rows {
		key := repositoryKey(row)
		i, ok := index[key]
		if !ok {
			i = len(groups)
			index[key] = i
			groups = append(groups, nil)
		}
		groups[i] = append(groups[i], row)
	}
	return groups
}

// readPRStates reads the state of every pull request rows track, all of
// one repository, in one forge request.
func (a *App) readPRStates(ctx context.Context, rows []store.ThreadPullRequest) (map[int]gitops.PRState, error) {
	prs := make([]gitops.TrackedPR, 0, len(rows))
	seen := make(map[int]struct{}, len(rows))
	for _, row := range rows {
		if _, dup := seen[row.Number]; dup {
			continue
		}
		seen[row.Number] = struct{}{}
		namespace, repo, err := gitops.SplitProjectForForge(row.Forge, row.Project)
		if err != nil {
			return nil, err
		}
		prs = append(prs, gitops.TrackedPR{
			Ref:    gitops.PRReference{Forge: row.Forge, Host: row.Host, Namespace: namespace, Repo: repo, Number: row.Number},
			NodeID: row.NodeID,
		})
	}
	ctx, cancel := context.WithTimeout(ctx, mergedPRForgeTimeout)
	defer cancel()
	return a.gitCore().ReadPRStates(ctx, prs)
}

// applyPRStates records what the forge said about each pull request rows
// track: a merge, or a close (or a pull request the forge no longer has),
// which retires it. An open one stays tracked. Reports whether a merge was
// recorded.
func (a *App) applyPRStates(rows []store.ThreadPullRequest, states map[int]gitops.PRState) bool {
	merged := false
	done := make(map[int]struct{}, len(rows))
	now := time.Now()
	for _, row := range rows {
		if _, dup := done[row.Number]; dup {
			continue
		}
		done[row.Number] = struct{}{}
		state, ok := states[row.Number]
		switch {
		case ok && state.State == "merged":
			at := state.MergedAt
			if at.IsZero() {
				at = now
			}
			if n, err := a.store.MarkThreadPullRequestsMerged(row.Forge, row.Host, row.Project, row.Number, at.UnixMilli()); err != nil {
				a.logMergedPR("merge:"+threadPullRequestKey(row), fmt.Sprintf("merged PR cleanup: %v", err))
			} else if n > 0 {
				merged = true
			}
		case !ok || state.State == "closed":
			if _, err := a.store.MarkThreadPullRequestsHandled(row.Forge, row.Host, row.Project, row.Number, store.PROutcomeClosed, now.UnixMilli()); err != nil {
				a.logMergedPR("close:"+threadPullRequestKey(row), fmt.Sprintf("merged PR cleanup: %v", err))
			}
		}
	}
	return merged
}

// mergedPRPoll reads, for every repository that is due, the state of the
// unmerged pull requests its threads track, in one request per repository.
// A repository with nothing tracked is never read. A row whose thread left
// its worktree is not read either: nothing would be removed for it.
func (a *App) mergedPRPoll(ctx context.Context, now time.Time) {
	c := &a.mergedPRs
	rows, err := a.store.ListPendingThreadPullRequests()
	if err != nil {
		a.logMergedPR("list", fmt.Sprintf("merged PR cleanup: %v", err))
		return
	}
	var unmerged []store.ThreadPullRequest
	for _, row := range rows {
		if row.MergedAt == 0 {
			unmerged = append(unmerged, row)
		}
	}
	live := make(map[string]struct{})
	for _, group := range groupByRepository(unmerged) {
		key := repositoryKey(group[0])
		live[key] = struct{}{}
		if !c.reads[key].due(now) {
			continue
		}
		group = slices.DeleteFunc(group, func(row store.ThreadPullRequest) bool {
			t, err := a.store.GetThread(row.ThreadID)
			if errors.Is(err, sql.ErrNoRows) {
				return true
			}
			if err == nil {
				var linked bool
				if linked, err = a.threadInLinkedWorktree(t, row.WorktreePath); err == nil {
					return !linked
				}
			}
			// Left out of this read only; the next poll judges it again.
			a.logMergedPR("poll-thread:"+row.ThreadID, fmt.Sprintf("merged PR cleanup: %v", err))
			return true
		})
		if len(group) == 0 {
			continue
		}
		states, err := a.readPRStates(ctx, group)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			backoffFor(&c.reads, key).fail(now, c.pollInterval(), err)
			a.logMergedPR("poll:"+key, fmt.Sprintf("merged PR cleanup: read %s pull requests: %v", group[0].Project, err))
			continue
		}
		*backoffFor(&c.reads, key) = mergedPRBackoff{next: now.Add(c.pollInterval())}
		a.clearMergedPRLog("poll:" + key)
		a.applyPRStates(group, states)
	}
	for key := range c.reads {
		if _, ok := live[key]; !ok {
			delete(c.reads, key)
		}
	}
}

// mergedPROutcome is what one pass did with a merged pull request.
type mergedPROutcome int

const (
	mergedPRWait mergedPROutcome = iota
	mergedPRSkipped
	mergedPRGone
	mergedPRKept
	mergedPRRemoved
)

func (o mergedPROutcome) store() string {
	switch o {
	case mergedPRRemoved:
		return store.PROutcomeRemoved
	case mergedPRKept:
		return store.PROutcomeKept
	case mergedPRGone:
		return store.PROutcomeGone
	default:
		return store.PROutcomeSkipped
	}
}

// mergedPRActPass acts on every merged pull request not yet handled.
func (a *App) mergedPRActPass(ctx context.Context) {
	c := &a.mergedPRs
	rows, err := a.store.ListPendingThreadPullRequests()
	if err != nil {
		a.logMergedPR("list", fmt.Sprintf("merged PR cleanup: %v", err))
		return
	}
	a.pruneNoneRead(rows)
	index := make(map[string]int)
	var groups [][]store.ThreadPullRequest
	for _, row := range rows {
		if row.MergedAt == 0 {
			continue
		}
		key := threadPullRequestKey(row)
		i, ok := index[key]
		if !ok {
			i = len(groups)
			index[key] = i
			groups = append(groups, nil)
		}
		groups[i] = append(groups[i], row)
	}
	// Retry schedules end with their pull request.
	for key := range c.removals {
		if _, ok := index[key[:strings.LastIndex(key, "\x00")]]; !ok {
			delete(c.removals, key)
		}
	}
	waiting := false
	for _, group := range groups {
		if ctx.Err() != nil {
			return
		}
		if a.actOnMergedPR(ctx, group) == mergedPRWait {
			waiting = true
		}
	}
	c.waitingIdle.Store(waiting)
}

// mergedPRSite is one worktree a merged pull request's threads run in.
type mergedPRSite struct {
	path      string
	project   string
	projectID string
	threadIDs []string
}

// actOnMergedPR acts on one merged pull request, rows being the threads
// that track it, and records the outcome on all of them unless it has to
// wait.
func (a *App) actOnMergedPR(ctx context.Context, rows []store.ThreadPullRequest) mergedPROutcome {
	pr := rows[0]
	var sites []*mergedPRSite
	for _, row := range rows {
		t, err := a.store.GetThread(row.ThreadID)
		if errors.Is(err, sql.ErrNoRows) {
			// Deleted: its row went with it.
			continue
		}
		var linked bool
		if err == nil {
			linked, err = a.threadInLinkedWorktree(t, row.WorktreePath)
		}
		if err != nil {
			// Unknown is not "left the worktree": ask again next pass.
			a.logMergedPR("act-thread:"+row.ThreadID, fmt.Sprintf("merged PR cleanup: %v", err))
			return mergedPRWait
		}
		a.clearMergedPRLog("act-thread:" + row.ThreadID)
		if !linked {
			continue
		}
		i := slices.IndexFunc(sites, func(site *mergedPRSite) bool { return gitops.SameFilesystemPath(site.path, row.WorktreePath) })
		if i < 0 {
			sites = append(sites, &mergedPRSite{path: row.WorktreePath, project: t.ProjectPath, projectID: t.ProjectID})
			i = len(sites) - 1
		}
		sites[i].threadIDs = append(sites[i].threadIDs, t.ID)
	}
	outcome := mergedPRSkipped
	for _, site := range sites {
		result := a.cleanUpMergedPRWorktree(ctx, pr, site)
		if result == mergedPRWait {
			return mergedPRWait
		}
		outcome = max(outcome, result)
	}
	if _, err := a.store.MarkThreadPullRequestsHandled(pr.Forge, pr.Host, pr.Project, pr.Number, outcome.store(), time.Now().UnixMilli()); err != nil {
		a.logMergedPR("handled:"+threadPullRequestKey(pr), fmt.Sprintf("merged PR cleanup: %v", err))
		return mergedPRWait
	}
	return outcome
}

// errMergedPRStale is the removal's lock-held recheck finding that the
// cleanup no longer applies: the worktree is on another branch, or a user
// wrote in one of its threads after the merge.
var errMergedPRStale = errors.New("the merged pull request's cleanup no longer applies")

// errMergedPRWorktreeGone is the lock-held recheck finding the worktree
// already gone.
var errMergedPRWorktreeGone = errors.New("the worktree is already gone")

// cleanUpMergedPRWorktree applies the cleanup's conditions to one worktree
// and removes it when they all hold. The local checks come first, so a
// busy worktree costs no forge request while it waits; those the forge
// lookup's wait could invalidate are made again with the threads locked,
// immediately before the removal.
func (a *App) cleanUpMergedPRWorktree(ctx context.Context, pr store.ThreadPullRequest, site *mergedPRSite) mergedPROutcome {
	c := &a.mergedPRs
	now := time.Now()
	label := prthread.Label(pr.Forge, pr.Number)
	name := filepath.Base(site.path)
	removalKey := threadPullRequestKey(pr) + "\x00" + site.path
	if gitops.SameFilesystemPath(site.path, site.project) {
		return mergedPRSkipped
	}
	if !c.removals[removalKey].due(now) {
		return mergedPRWait
	}
	worktree, ok, err := a.findWorktree(site.project, site.path)
	if err != nil {
		a.logMergedPR("find:"+site.path, fmt.Sprintf("merged PR cleanup: %s: %v", site.path, err))
		return mergedPRWait
	}
	if !ok {
		// Already gone: the registry watch moves its threads as it does
		// for any removal made outside the app.
		return mergedPRGone
	}
	if info, statErr := os.Stat(site.path); errors.Is(statErr, fs.ErrNotExist) {
		return mergedPRGone
	} else if statErr != nil || !info.IsDir() {
		if statErr == nil {
			statErr = errors.New("not a directory")
		}
		a.logMergedPR("stat:"+site.path, fmt.Sprintf("merged PR cleanup: %s: %v", site.path, statErr))
		return mergedPRWait
	}
	a.clearMergedPRLog("stat:" + site.path)
	if worktree.Branch != pr.Branch {
		return mergedPRSkipped
	}
	occupants, err := a.threadsReferencingWorkspace(site.path)
	if err != nil {
		a.logMergedPR("occupants:"+site.path, fmt.Sprintf("merged PR cleanup: %v", err))
		return mergedPRWait
	}
	slices.Sort(occupants)
	occupants = slices.Compact(occupants)
	if a.mergedPRWorktreeBusy(site.path, occupants) != nil {
		return mergedPRWait
	}
	// The threads a notice can be written to: a conversation moved to
	// another computer keeps its rows as they were.
	told, err := a.mutableWorkspaceThreads(occupants)
	if err != nil {
		return mergedPRWait
	}
	if after, err := a.userWroteAfter(occupants, pr.MergedAt); err != nil {
		a.logMergedPR("message:"+site.path, fmt.Sprintf("merged PR cleanup: %v", err))
		return mergedPRWait
	} else if after {
		return mergedPRSkipped
	}

	repo := repositoryKey(pr)
	if !c.lookups[repo].due(now) {
		return mergedPRWait
	}
	lookup, cancel := context.WithTimeout(ctx, mergedPRForgeTimeout)
	open, err := a.gitCore().ListOpenPRs(lookup, site.path, pr.Branch)
	cancel()
	if err != nil {
		if ctx.Err() == nil {
			backoffFor(&c.lookups, repo).fail(now, c.tick(), err)
			a.logMergedPR("open:"+repo, fmt.Sprintf("merged PR cleanup: open PRs of %s: %v", pr.Branch, err))
		}
		return mergedPRWait
	}
	a.clearMergedPRLog("open:" + repo)
	for _, other := range open {
		if other.Number == pr.Number {
			// The forge still lists the merged one as open; ask again later.
			backoffFor(&c.lookups, repo).fail(now, c.tick(), nil)
			return mergedPRWait
		}
	}
	delete(c.lookups, repo)
	if len(open) > 0 {
		for _, id := range site.threadIDs {
			if err := a.recordOpenPR(id, site.path, pr.Branch, open[0]); err != nil {
				a.logMergedPR("record:"+id, fmt.Sprintf("merged PR cleanup: %v", err))
				return mergedPRWait
			}
		}
		return mergedPRSkipped
	}
	status, err := a.worktreeApplication().Status(site.project, site.path)
	if err != nil {
		a.logMergedPR("status:"+site.path, fmt.Sprintf("merged PR cleanup: %v", err))
		return mergedPRWait
	}
	if status.UncommittedCount > 0 {
		a.postMergedPRKept(told, label, name, uncommittedFiles(status.UncommittedCount))
		return mergedPRKept
	}
	_, err = a.removeProjectWorktree(site.projectID, site.project, site.path, worktreeRemovalPolicy{
		keepsBranch: true,
		mergedPR:    label,
		idle: func(locked []string) error {
			return a.mergedPRStillDue(pr, site, occupants, locked)
		},
	})
	if err == nil {
		delete(c.removals, removalKey)
		return mergedPRRemoved
	}
	return a.mergedPRRemovalFailed(pr, site, told, err, now)
}

// mergedPRRemovalFailed is the outcome of a removal of site that returned
// err: a recheck that found the cleanup no longer applies or the worktree
// gone, busy threads, uncommitted changes, a removal git completed whose
// reaction failed, a worktree removed outside the app meanwhile, or a
// removal git refused, which is told once and retried after a backoff.
func (a *App) mergedPRRemovalFailed(pr store.ThreadPullRequest, site *mergedPRSite, told []string, err error, now time.Time) mergedPROutcome {
	c := &a.mergedPRs
	label := prthread.Label(pr.Forge, pr.Number)
	name := filepath.Base(site.path)
	removalKey := threadPullRequestKey(pr) + "\x00" + site.path
	var busy *errWorktreeBusy
	var dirty *errWorktreeDirty
	var followUp *errWorktreeRemovedFollowUp
	switch {
	case errors.Is(err, errMergedPRStale):
		return mergedPRSkipped
	case errors.Is(err, errMergedPRWorktreeGone):
		return mergedPRGone
	case errors.As(err, &busy):
		return mergedPRWait
	case errors.As(err, &dirty):
		a.postMergedPRKept(told, label, name, uncommittedFiles(dirty.count))
		return mergedPRKept
	case errors.As(err, &followUp):
		// git removed it; what failed after is reported on the threads
		// it concerns.
		for _, id := range told {
			a.emitErrorToThread(id, fmt.Sprintf("%s merged and worktree %s was removed, but %v", label, name, followUp.err))
		}
		return mergedPRRemoved
	}
	if _, still, findErr := a.findWorktree(site.project, site.path); findErr == nil && !still {
		// Removed outside the app while this one ran: the registry watch
		// moves its threads and tells them.
		return mergedPRGone
	}
	if recheck, statusErr := a.worktreeApplication().Status(site.project, site.path); statusErr == nil && recheck.UncommittedCount > 0 {
		a.postMergedPRKept(told, label, name, uncommittedFiles(recheck.UncommittedCount))
		return mergedPRKept
	}
	// git's error names paths; the notice names the worktree only, once.
	// The removal is tried again later; when it succeeds the threads get
	// the usual notice.
	log.Printf("merged PR cleanup: remove %s: %v", site.path, err)
	backoffFor(&c.removals, removalKey).fail(now, c.tick(), nil)
	first, markErr := a.store.MarkThreadPullRequestsRemovalFailed(pr.Forge, pr.Host, pr.Project, pr.Number, now.UnixMilli())
	if markErr != nil {
		a.logMergedPR("removal-failed:"+removalKey, fmt.Sprintf("merged PR cleanup: %v", markErr))
	} else if first {
		a.postMergedPRKept(told, label, name, "removing it failed")
	}
	return mergedPRWait
}

// mergedPRStillDue is the removal's lock-held recheck: the conditions the
// cleanup checked before its forge lookup, made again with every thread in
// the worktree locked so none can change before git runs. checked is the
// thread set those checks covered.
func (a *App) mergedPRStillDue(pr store.ThreadPullRequest, site *mergedPRSite, checked, locked []string) error {
	if !slices.Equal(checked, locked) {
		return fmt.Errorf("the threads in worktree %s changed", site.path)
	}
	if err := a.mergedPRWorktreeBusy(site.path, locked); err != nil {
		return err
	}
	worktree, ok, err := a.findWorktree(site.project, site.path)
	if err != nil {
		return err
	}
	if !ok {
		return errMergedPRWorktreeGone
	}
	if worktree.Branch != pr.Branch {
		return errMergedPRStale
	}
	after, err := a.userWroteAfter(locked, pr.MergedAt)
	if err != nil {
		return err
	}
	if after {
		return errMergedPRStale
	}
	return nil
}

// userWroteAfter reports whether a user wrote in any of threadIDs after
// at (unix milliseconds).
func (a *App) userWroteAfter(threadIDs []string, at int64) (bool, error) {
	for _, id := range threadIDs {
		latest, found, err := a.store.LatestHumanUserMessageAt(id)
		if err != nil {
			return false, err
		}
		if found && latest > at {
			return true, nil
		}
	}
	return false, nil
}

func uncommittedFiles(count int) string {
	if count == 1 {
		return "1 uncommitted file"
	}
	return fmt.Sprintf("%d uncommitted files", count)
}

// postMergedPRKept tells each thread in a worktree the cleanup kept why it
// stays.
func (a *App) postMergedPRKept(threadIDs []string, label, name, reason string) {
	text := fmt.Sprintf("%s merged. Kept worktree %s: %s.", label, name, reason)
	for _, id := range threadIDs {
		if err := a.emitNoticeToThread(id, noticeKindWarning, text); err != nil {
			log.Printf("merged PR cleanup: %v", err)
		}
	}
}

// mergedPRWorktreeBusy says why a worktree's threads are not idle, or nil:
// a running turn or background task, a pending approval or question, a
// queued send, a setup recipe running in it, or a transfer deciding where a
// thread lives.
func (a *App) mergedPRWorktreeBusy(path string, threadIDs []string) error {
	if err := a.ensureWorkspaceChangeAllowed("remove this worktree", path); err != nil {
		return err
	}
	if a.worktreeSetupService().HasRunningPath(path) {
		return fmt.Errorf("worktree %s is running its setup", path)
	}
	for _, id := range threadIDs {
		if _, err := a.threadApplication().CheckCleanup(id); err != nil {
			return err
		}
		if a.triage == nil {
			continue
		}
		live := a.triage.LiveStateSnapshotForThread(id)
		switch {
		case live.ActiveTurn != nil:
			return fmt.Errorf("thread %s is responding", id)
		case len(live.QueueItems)+len(live.FlushedItems)+len(live.DeferredItems) > 0:
			return fmt.Errorf("thread %s has queued messages", id)
		case len(live.Interactive.Approvals)+len(live.Interactive.UserInputs) > 0:
			return fmt.Errorf("thread %s is waiting for an answer", id)
		}
	}
	return nil
}

// logMergedPR logs message once per key until the message changes or the
// key is cleared, so a failure repeated every tick is one log line.
func (a *App) logMergedPR(key, message string) {
	c := &a.mergedPRs
	c.mu.Lock()
	if c.logged == nil {
		c.logged = make(map[string]string)
	}
	repeat := c.logged[key] == message
	c.logged[key] = message
	if len(c.logged) > 256 {
		clear(c.logged)
	}
	c.mu.Unlock()
	if !repeat {
		log.Print(message)
	}
}

func (a *App) clearMergedPRLog(key string) {
	c := &a.mergedPRs
	c.mu.Lock()
	delete(c.logged, key)
	c.mu.Unlock()
}
