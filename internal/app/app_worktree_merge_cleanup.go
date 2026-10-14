package app

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"slices"
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
// the branch, and each thread that moved to Base is told.

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

// mergedPRCleanupState is the cleanup's worker. observed and act are
// handed over under mu; nextPoll, failures, noneRead and logged belong to
// the worker goroutine.
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

	nextPoll map[string]time.Time
	failures map[string]int
	noneRead map[string]string
	logged   map[string]string
}

func (c *mergedPRCleanupState) pollInterval() time.Duration {
	if c.interval > 0 {
		return c.interval
	}
	return mergedPRPollInterval
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
		if a.handleOpenPRObservation(ctx, observed[key]) {
			act = true
		}
	}
	if act {
		a.mergedPRActPass(ctx)
	}
}

// linkedWorktreeThreadsAt returns the threads running in path as their
// linked worktree. A thread in its project root is never one.
func (a *App) linkedWorktreeThreadsAt(path string) []store.Thread {
	ids, err := a.threadsReferencingWorkspace(path)
	if err != nil {
		a.logMergedPR("occupants:"+path, fmt.Sprintf("merged PR cleanup: threads in %s: %v", path, err))
		return nil
	}
	slices.Sort(ids)
	ids = slices.Compact(ids)
	threads := make([]store.Thread, 0, len(ids))
	for _, id := range ids {
		t, err := a.store.GetThread(id)
		if err != nil {
			continue
		}
		if a.threadInLinkedWorktree(t, path) {
			threads = append(threads, t)
		}
	}
	return threads
}

// threadInLinkedWorktree reports whether t runs in path as a linked
// worktree of its project.
func (a *App) threadInLinkedWorktree(t store.Thread, path string) bool {
	if !gitops.SameFilesystemPath(t.WorktreePath, path) || !gitops.SameFilesystemPath(t.WorkspacePath, path) {
		return false
	}
	project, _, err := a.gitApplication().ResolveThreadPaths(t)
	return err == nil && !gitops.SameFilesystemPath(project, path)
}

// handleOpenPRObservation records an open pull request for every thread in
// the observed checkout, or, when the checkout's branch has none, reads the
// state of the pull request its threads track once per disappearance.
// Reports whether a merge was recorded.
func (a *App) handleOpenPRObservation(ctx context.Context, obs gitops.OpenPRObservation) bool {
	threads := a.linkedWorktreeThreadsAt(obs.Cwd)
	if len(threads) == 0 {
		return false
	}
	c := &a.mergedPRs
	if c.noneRead == nil {
		c.noneRead = make(map[string]string)
	}
	if obs.PR != nil {
		for _, t := range threads {
			a.recordOpenPR(t.ID, obs.Cwd, obs.Branch, *obs.PR)
			delete(c.noneRead, t.ID)
		}
		return false
	}
	var due []store.ThreadPullRequest
	for _, t := range threads {
		row, ok, err := a.store.GetThreadPullRequest(t.ID)
		if err != nil {
			a.logMergedPR("row:"+t.ID, fmt.Sprintf("merged PR cleanup: %v", err))
			continue
		}
		if !ok || row.HandledAt != 0 || row.MergedAt != 0 || row.Branch != obs.Branch || !gitops.SameFilesystemPath(row.WorktreePath, obs.Cwd) {
			continue
		}
		key := threadPullRequestKey(row)
		if c.noneRead[t.ID] == key {
			continue
		}
		c.noneRead[t.ID] = key
		due = append(due, row)
	}
	merged := false
	for _, group := range groupByRepository(due) {
		states, err := a.readPRStates(ctx, group)
		if err != nil {
			a.logMergedPR("read:"+repositoryKey(group[0]), fmt.Sprintf("merged PR cleanup: read %s: %v", group[0].URL, err))
			continue
		}
		if a.applyPRStates(group, states) {
			merged = true
		}
	}
	return merged
}

// recordOpenPR tracks pr as the open pull request of threadID's worktree.
func (a *App) recordOpenPR(threadID, worktree, branch string, pr gitops.GitPR) {
	ref, err := gitops.ParsePRURL(pr.URL)
	if err != nil {
		a.logMergedPR("url:"+pr.URL, fmt.Sprintf("merged PR cleanup: open PR %q: %v", pr.URL, err))
		return
	}
	if ref.Forge == "github" && pr.ID == "" {
		a.logMergedPR("node:"+pr.URL, fmt.Sprintf("merged PR cleanup: GitHub reported no node id for %s", pr.URL))
		return
	}
	if _, err := a.store.RecordOpenThreadPullRequest(store.ThreadPullRequest{
		ThreadID: threadID, Forge: ref.Forge, Host: ref.Host, Project: ref.Project(), Number: ref.Number,
		URL: pr.URL, NodeID: pr.ID, Branch: branch, WorktreePath: worktree, SeenOpenAt: time.Now().UnixMilli(),
	}); err != nil {
		a.logMergedPR("record:"+threadID, fmt.Sprintf("merged PR cleanup: %v", err))
	}
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
	if c.nextPoll == nil {
		c.nextPoll = make(map[string]time.Time)
		c.failures = make(map[string]int)
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
		if now.Before(c.nextPoll[key]) {
			continue
		}
		group = slices.DeleteFunc(group, func(row store.ThreadPullRequest) bool {
			t, err := a.store.GetThread(row.ThreadID)
			return err != nil || !a.threadInLinkedWorktree(t, row.WorktreePath)
		})
		if len(group) == 0 {
			continue
		}
		states, err := a.readPRStates(ctx, group)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			if limited, ok := errors.AsType[*forgeapi.RateLimitedError](err); ok {
				// The forge's gate says when it answers again; asking
				// before then costs nothing but would log.
				c.nextPoll[key] = limited.Until
			} else {
				c.failures[key]++
				c.nextPoll[key] = now.Add(min(c.pollInterval()<<min(c.failures[key], 8), max(mergedPRPollBackoffMax, c.pollInterval())))
			}
			a.logMergedPR("poll:"+key, fmt.Sprintf("merged PR cleanup: read %s pull requests: %v", group[0].Project, err))
			continue
		}
		c.failures[key] = 0
		c.nextPoll[key] = now.Add(c.pollInterval())
		a.clearMergedPRLog("poll:" + key)
		a.applyPRStates(group, states)
	}
	for key := range c.nextPoll {
		if _, ok := live[key]; !ok {
			delete(c.nextPoll, key)
			delete(c.failures, key)
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
		if err != nil || !a.threadInLinkedWorktree(t, row.WorktreePath) {
			continue
		}
		project, _, err := a.gitApplication().ResolveThreadPaths(t)
		if err != nil {
			continue
		}
		i := slices.IndexFunc(sites, func(site *mergedPRSite) bool { return gitops.SameFilesystemPath(site.path, row.WorktreePath) })
		if i < 0 {
			sites = append(sites, &mergedPRSite{path: row.WorktreePath, project: project, projectID: t.ProjectID})
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

// cleanUpMergedPRWorktree applies the cleanup's conditions to one worktree
// and removes it when they all hold. The local checks come first, so a
// busy worktree costs no forge request while it waits.
func (a *App) cleanUpMergedPRWorktree(ctx context.Context, pr store.ThreadPullRequest, site *mergedPRSite) mergedPROutcome {
	label := prthread.Label(pr.Forge, pr.Number)
	name := filepath.Base(site.path)
	if gitops.SameFilesystemPath(site.path, site.project) {
		return mergedPRSkipped
	}
	worktree, ok, err := a.findWorktree(site.project, site.path)
	if err != nil {
		a.logMergedPR("find:"+site.path, fmt.Sprintf("merged PR cleanup: %s: %v", site.path, err))
		return mergedPRWait
	}
	if info, statErr := os.Stat(site.path); !ok || statErr != nil || !info.IsDir() {
		// Already gone: the registry watch moves its threads as it does for
		// any removal made outside the app. Drop git's registration of a
		// directory that is no longer there.
		if err := a.gitCore().PruneWorktrees(site.project); err != nil {
			a.logMergedPR("prune:"+site.project, fmt.Sprintf("merged PR cleanup: %v", err))
		}
		return mergedPRGone
	}
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
	for _, id := range occupants {
		at, found, err := a.store.LatestHumanUserMessageAt(id)
		if err != nil {
			a.logMergedPR("message:"+id, fmt.Sprintf("merged PR cleanup: %v", err))
			return mergedPRWait
		}
		if found && at > pr.MergedAt {
			return mergedPRSkipped
		}
	}
	lookup, cancel := context.WithTimeout(ctx, mergedPRForgeTimeout)
	open, err := a.gitCore().ListOpenPRs(lookup, site.path, pr.Branch)
	cancel()
	if err != nil {
		a.logMergedPR("open:"+site.path, fmt.Sprintf("merged PR cleanup: open PRs of %s: %v", pr.Branch, err))
		return mergedPRWait
	}
	for _, other := range open {
		if other.Number == pr.Number {
			// The forge still lists the merged one as open; ask again later.
			return mergedPRWait
		}
	}
	if len(open) > 0 {
		for _, id := range site.threadIDs {
			a.recordOpenPR(id, site.path, pr.Branch, open[0])
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
			return a.mergedPRWorktreeBusy(site.path, locked)
		},
	})
	var busy *errWorktreeBusy
	var dirty *errWorktreeDirty
	switch {
	case err == nil:
		a.clearMergedPRLog("remove:" + site.path)
		return mergedPRRemoved
	case errors.As(err, &busy):
		return mergedPRWait
	case errors.As(err, &dirty):
		a.postMergedPRKept(told, label, name, uncommittedFiles(dirty.count))
		return mergedPRKept
	}
	if _, still, findErr := a.findWorktree(site.project, site.path); findErr == nil && !still {
		// git removed it; what failed came after, and the threads it
		// concerns are told.
		for _, id := range told {
			a.emitErrorToThread(id, fmt.Sprintf("%s merged and worktree %s was removed, but %v", label, name, err))
		}
		return mergedPRRemoved
	}
	if recheck, statusErr := a.worktreeApplication().Status(site.project, site.path); statusErr == nil && recheck.UncommittedCount > 0 {
		a.postMergedPRKept(told, label, name, uncommittedFiles(recheck.UncommittedCount))
		return mergedPRKept
	}
	// git's error names paths; the notice names the worktree only.
	log.Printf("merged PR cleanup: remove %s: %v", site.path, err)
	a.postMergedPRKept(told, label, name, "removing it failed")
	return mergedPRKept
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
