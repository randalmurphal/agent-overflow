package main

import (
	"context"
	"log"
	"slices"
	"sync"
	"time"
)

// Background git fetch. Without it `BehindCount` is stale forever unless
// the user fetches by hand, which makes the sidebar's behind badge
// decorative rather than informative.
//
// Shape (settings: BackgroundGitFetch, default on):
//
//   - Scope is the repositories behind the projects the sidebar
//     currently lists — not threads, not panes, not worktrees. A project
//     the user removed stops being fetched on the next tick.
//   - One fetch per underlying REPOSITORY, keyed on the resolved git
//     common dir, so N worktrees of one repo never fan out into N
//     fetches. The dedup happens twice on purpose: here (so the pass
//     visits each repo once and the error memo has one entry per repo)
//     and inside gitops.FetchRemotesBackground (so the picker's warm-up
//     and this cadence share one window and one in-flight fetch).
//   - The fetch itself is `git fetch --quiet origin` under the package
//     default non-interactive env — structurally unable to raise a
//     credential prompt. See internal/git/fetch_background.go.
//   - Failures are expected for a whole class of users (private remote,
//     no ambient credentials, laptop on a plane). They are memoized per
//     repo and logged once per distinct message, never toasted: this is
//     a cadence nobody asked for, and it must not interrupt anyone.
//
// The refresh after a successful fetch needs no explicit trigger:
// `git fetch` writes `refs/remotes/<remote>/*` and FETCH_HEAD inside the
// common dir, and gitwatch already watches the git dir non-recursively
// plus refs/ recursively (internal/git/watch_roots.go, gitMetadataRoots)
// for every subscribed workspace — including linked worktrees, which
// watch the shared common dir. Verified end to end by
// TestBackgroundFetchRefUpdateReachesSubscribers in internal/gitwatch.
//
// Lifecycle takes both halves of the two existing patterns, because it
// needs both: the stop channel + WaitGroup of startRetentionCleanup, so
// Shutdown joins the goroutine before the store it reads from closes,
// AND a context derived from lifeCtx (the sysstat sampler's half) so
// cancelling actually kills the `git fetch` in flight instead of waiting
// out its 45s subprocess timeout on a dead network.

const (
	// backgroundFetchInitialDelay defers the first pass past boot. The
	// first paint is competing for the same git binary (initial status
	// for every open pane), and remote-tracking refs that were stale for
	// hours are not urgent to the second.
	backgroundFetchInitialDelay = 45 * time.Second

	// backgroundFetchTickInterval is the CADENCE, not the rate limit —
	// gitops.FetchStaleWindow (5 min) is the rate limit, and it is
	// shared with the branch picker's warm-up so the two can't
	// double-fetch. The tick is deliberately much shorter than that
	// window: ticking at exactly the window means every tick lands a
	// hair before the previous fetch's stamp expires (the stamp is
	// written when the fetch *finishes*), so every other tick skips and
	// the effective cadence silently doubles. A short tick also picks up
	// a just-added project without waiting out a whole window.
	//
	// Cheap by construction: a tick that has nothing to do costs one map
	// lookup per project (the common dir is memoized in gitops.Core, and
	// the freshness check is a map read) and spawns no subprocess.
	backgroundFetchTickInterval = 1 * time.Minute

	// Error-memo key namespaces. A pass remembers failures against three
	// different kinds of subject — the store read, a project path that
	// doesn't resolve to a repository, and a repository (its common dir)
	// — and all three are strings that could otherwise collide (a bare
	// repo added as a project IS its own common dir). Prefixing keeps
	// them distinct without a second map.
	backgroundFetchStoreKey = "store"
	backgroundFetchPathKey  = "path:"
	backgroundFetchRepoKey  = "repo:"
)

// backgroundFetchErrorMemo remembers the last failure reported per
// repository so a permanently-unreachable remote logs once instead of
// every tick, while a NEW failure mode still gets a line.
//
// The state transitions matter more than the states: a success must
// clear the memo (so the same error recurring later is news again), and
// a repo leaving the project list must drop its entry (so re-adding it
// reports honestly). Both are covered in app_git_background_fetch_test.go.
type backgroundFetchErrorMemo struct {
	mu   sync.Mutex
	last map[string]string
}

// shouldLog records message as key's latest failure and reports whether
// it differs from the last one recorded for that key.
func (m *backgroundFetchErrorMemo) shouldLog(key, message string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.last == nil {
		m.last = make(map[string]string)
	}
	if previous, ok := m.last[key]; ok && previous == message {
		return false
	}
	m.last[key] = message
	return true
}

// clear forgets key's last failure, so its next failure logs again.
// Called on every success — including a success that follows a long run
// of identical failures.
func (m *backgroundFetchErrorMemo) clear(key string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.last, key)
}

// retain drops every remembered key outside live, bounding the memo by
// the current project set rather than by everything ever seen.
func (m *backgroundFetchErrorMemo) retain(live map[string]struct{}) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for key := range m.last {
		if _, ok := live[key]; !ok {
			delete(m.last, key)
		}
	}
}

// startBackgroundGitFetch launches the fetch cadence. Idempotent: a
// second call while one is running is a no-op, so repeated
// ServiceStartup in test fixtures can't fan out goroutines.
//
// Never starts when backgroundFetchDisabled is set (harness mode), and
// never runs from unit tests, which build *App directly and never call
// Start. Both matter: this is the one background loop that runs git
// against whatever repositories the store happens to name, and a test
// suite that reached a developer's real repository would be running
// network commands nobody asked for.
func (a *App) startBackgroundGitFetch() {
	if a.backgroundFetchDisabled {
		return
	}

	a.mu.Lock()
	if a.backgroundFetchStop != nil {
		a.mu.Unlock()
		return
	}
	stop := make(chan struct{})
	// Derived from the app-lifetime ctx so BOTH shutdown paths kill an
	// in-flight `git fetch`: appCtx cancellation (Shutdown step 1) and
	// stopBackgroundGitFetch's own cancel. Without it a fetch hanging on
	// a dead network could hold teardown for the subprocess timeout.
	ctx, cancel := context.WithCancel(a.lifeCtx())
	a.backgroundFetchStop = stop
	a.backgroundFetchCancel = cancel
	// Add(1) under the lock for the same memory-model reason as the
	// retention sweeper: a concurrent stop that sees a non-nil channel
	// must observe the counter at 1 before it waits.
	a.backgroundFetchWG.Add(1)
	a.mu.Unlock()

	go func() {
		defer a.backgroundFetchWG.Done()
		defer cancel()

		initial := time.NewTimer(backgroundFetchInitialDelay)
		defer initial.Stop()
		select {
		case <-stop:
			return
		case <-initial.C:
			a.runBackgroundFetchPass(ctx)
		}

		ticker := time.NewTicker(backgroundFetchTickInterval)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				a.runBackgroundFetchPass(ctx)
			}
		}
	}()
}

// stopBackgroundGitFetch signals the goroutine to exit and waits for it
// to return. Safe before start and safe to call twice.
func (a *App) stopBackgroundGitFetch() {
	a.mu.Lock()
	stop := a.backgroundFetchStop
	cancel := a.backgroundFetchCancel
	a.backgroundFetchStop = nil
	a.backgroundFetchCancel = nil
	a.mu.Unlock()
	if stop != nil {
		close(stop)
	}
	// Cancel before waiting: a pass parked in `git fetch` exits on the
	// killed subprocess instead of holding teardown for its timeout.
	if cancel != nil {
		cancel()
	}
	a.backgroundFetchWG.Wait()
}

// runBackgroundFetchPass performs one pass over the sidebar's projects.
// Reads the settings toggle live, so switching it off takes effect at
// the next tick without a restart.
//
// ctx aborts the pass between repositories AND kills the `git fetch`
// subprocess of the repository in flight when it is cancelled.
// Package-visible so tests can drive exactly one pass without spinning
// the ticker.
func (a *App) runBackgroundFetchPass(ctx context.Context) {
	if a.shuttingDown.Load() || a.store == nil || a.settings == nil {
		return
	}
	if !a.settings.Get().BackgroundGitFetch {
		return
	}

	projects, err := a.store.ListProjects()
	if err != nil {
		// Memoized like a fetch failure, and for the same reason: a
		// store that fails this read fails it every tick.
		if a.backgroundFetchErrors.shouldLog(backgroundFetchStoreKey, err.Error()) {
			log.Printf("git background fetch: list projects: %v", err)
		}
		return
	}
	a.backgroundFetchErrors.clear(backgroundFetchStoreKey)

	core := a.gitCore()
	// Sorted so the repository a group of worktrees is fetched *through*
	// is stable across passes; ListProjects is name-ordered, which a
	// rename would reshuffle.
	paths := make([]string, 0, len(projects))
	for _, project := range projects {
		if project.Path != "" {
			paths = append(paths, project.Path)
		}
	}
	slices.Sort(paths)

	// live collects what this pass actually visited, and prunes the memo
	// at the end. Every early return below skips that prune on purpose: a
	// partial pass has a partial live set, and pruning against it would
	// forget repositories it simply never got to.
	live := make(map[string]struct{}, len(paths))
	for _, path := range paths {
		if ctx.Err() != nil || a.shuttingDown.Load() {
			return
		}

		// A project path that isn't a repository (deleted, or a
		// directory the user pointed at before it was cloned) resolves
		// to no common dir. Memoize on the path — there is no repo
		// identity to memoize on — so it reports once, not every tick.
		commonDir, err := core.CommonDir(path)
		if err != nil {
			pathKey := backgroundFetchPathKey + path
			live[pathKey] = struct{}{}
			if a.backgroundFetchErrors.shouldLog(pathKey, err.Error()) {
				log.Printf("git background fetch: skipping %s: %v", path, err)
			}
			continue
		}

		repoKey := backgroundFetchRepoKey + commonDir
		if _, seen := live[repoKey]; seen {
			continue
		}
		live[repoKey] = struct{}{}

		if _, err := core.FetchRemotesBackground(ctx, path); err != nil {
			if a.backgroundFetchErrors.shouldLog(repoKey, err.Error()) {
				log.Printf("git background fetch: %s: %v", commonDir, err)
			}
			continue
		}
		// Success — including a no-op skip inside the stale window —
		// means the last failure is no longer the current state.
		a.backgroundFetchErrors.clear(repoKey)
	}

	a.backgroundFetchErrors.retain(live)
}
