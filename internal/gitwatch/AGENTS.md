# internal/gitwatch/

Live git status streams per workspace, backed by recursive filesystem
watching with a polling fallback. One `Manager` per app; subscribers
share a watcher per canonical cwd via refcount.

## Layout

- `doc.go` — package comment.
- `manager.go` — `Manager` type + `Subscribe` / `Close` API and
  refcount bookkeeping.
- `watcher.go` — per-cwd `workspaceWatcher` + `Subscription`. Owns the
  debounce timer, polling fallback, and subscriber fan-out.
- `watcher_rebuild.go` — watch-root staleness: per-event rebuild
  triggers (`inspectEvent`) and the refresh-edge recompute/reinstall
  (`maybeRebuildWatches`), including the forced reinstall that re-arms
  dead watchpoints after a root dir is deleted and recreated.
- `paths.go` — path canonicalization and watch-root normalization so
  `/tmp` symlinks don't spawn duplicate watchers on macOS, and nested
  git metadata roots are pruned before watcher installation.

## How it works

1. `Manager.Subscribe(cwd)` canonicalizes the path, fetches the initial
   `GitStatus` via the injected `StatusFn`, and either reuses an
   existing watcher for that cwd or starts a new one.
2. Before installing the watcher, `Manager` asks the injected
   `WatchRootsFn` for the workspace plus any git metadata directories
   that should trigger refreshes. Each root declares whether it is
   recursive. Linked worktrees need this because their `.git` file
   points to metadata under the main repo's `.git/worktrees/` directory;
   watching only `cwd/...` misses commits, index writes, and ref updates
   performed outside the app. The git package prunes ignored subtrees
   from the workspace side (see `internal/git/watch_roots.go`), so the
   root list is many small roots rather than one recursive `cwd/...`.
3. The watcher attaches `rjeczalik/notify` at each normalized root and
   uses the `path/...` recursive suffix only for roots that requested it.
   A trailing-edge 250ms debounce coalesces bursts, then re-runs
   `StatusFn` and broadcasts to subscribers.
4. While draining an event burst, the watcher inspects paths for
   root-set staleness (see `watcher_rebuild.go`): a `.gitignore`
   change; an index / exclude / config write under a `KindGitMeta`
   root (`git add -f` moves pruned boundaries via the index); an event
   for a root's `TriggerFile` (the global ignore file); or a new
   directory directly under a `KindAncestor` / `KindGitMeta` root (a
   new sibling dir there is covered by no root). At the next refresh
   edge it recomputes roots via the manager's pipeline and reinstalls
   only when they differ — EXCEPT when a current root's directory was
   recreated or the event queue hit capacity (drops possible): those
   force a reinstall even with identical roots, because a deleted
   root's notify watchpoint dies permanently and only a fresh install
   re-arms it. Recompute failure keeps the existing watches and leaves
   the rebuild flag set so any later refresh edge retries; reinstall
   failure (watches already stopped) escalates to polling, and a later
   reinstall that succeeds stops the then-redundant ticker. Watchers
   start rebuild-flagged to close the compute-vs-install subscribe
   race. run()'s deferred unregister (before `done` closes) guarantees
   `stop()` never races a rebuild's reinstall into leaked watches.
   Event-driven dead-watchpoint recovery needs the recreate event to
   arrive via a watched parent — a root whose parent is unwatched
   (the global-ignore dir, a linked worktree's private gitdir, cwd
   itself) stays event-dead after delete+recreate; the silent-death
   layer (item 8) is what bounds that and every other deaf-watch mode.
5. `lastStatus` is compared with `GitStatus.Equal` — unchanged status
   does not emit, keeping the wire quiet during heavy fs activity that
   doesn't affect the working tree (build outputs, ignored files).
6. If `notify.Watch` fails at install time (most commonly Linux
   inotify watch-limit exhaustion), the watcher transparently falls
   back to polling at `pollFallbackInterval`. Subscribers see the same
   `Updates()` shape; only cadence changes.
7. Subscribers receive a single buffered channel; on overflow the
   newest status supersedes the older one (the run loop drains the
   pending value before sending).
8. **Silent-death recovery.** An fs-watch install can "succeed" and
   then never deliver — observed 2026-08-01 on macOS: FSEvents streams
   installed during a dark-wake died when the machine re-slept,
   freezing the header diff badge for a whole session — and every
   rebuild trigger above rides on fs events, so a fully deaf watcher
   cannot heal itself. Two layers close the loop, both keyed on "the
   event stream has been quiet" so they cost nothing while events flow:
   - A `requestRefresh` (subscriber attach, post-action refresh) whose
     refresh observes a **non-PR** status change with no fs event
     inside `livenessQuietAfterEvent` proves the watches missed it:
     the watcher force-reinstalls. PR-field-only deltas are excluded —
     the attach hook exists to warm the PR cache, and a remote PR
     appearing says nothing about local watchpoints.
   - A `watchLivenessInterval` (60s) ticker probes with the fast
     (network-free) status fn, comparing ignoring PR fields. Ticks are
     skipped when any fs event arrived within the interval or while
     fallback polling owns refreshes, so the probe only ever runs
     against an idle-looking workspace. On drift it force-reinstalls
     and broadcasts the fresh status.

## Responsibility boundary

- What BELONGS here:
  - Watching a workspace path and turning fs activity into deduplicated
    `GitStatus` updates.
  - Watching git metadata roots supplied by the git package so linked
    worktree commits refresh the same workspace status stream.
  - Respecting recursive vs non-recursive root intent. Git dirs
    (primary and common alike) are intentionally watched non-recursively
    so `objects/` and pack churn do not explode watcher count or refresh
    volume, and ignored workspace subtrees are pruned entirely.
  - Refcounting watchers so multiple panes/threads pointing at the same
    workspace share a single fs watcher and a single git status pump.
- What does NOT belong here:
  - Resolving threads → workspace paths. The caller (App) owns thread
    lookup and decides which cwd to subscribe.
  - Wire transport. Subscribers consume `Updates()` and translate to
    whatever event channel they use; this package knows nothing about
    WebSockets.

## Anti-patterns

- Do NOT call `StatusFn` while holding `Manager.mu` or `workspaceWatcher.mu`.
  It shells out to git and can take tens of milliseconds.
- Do NOT close a `Subscription`'s channel from outside the watcher's
  run loop / `removeSubscriber`. The run loop is the sole writer.
- Do NOT block in the run loop. It serializes all updates for one cwd;
  a stuck run loop blocks every subscriber on that cwd.

## Testing

- Inject `Manager.installFn` to force the polling-fallback path without
  exhausting real OS limits.
- Inject `StatusFn` to control the status sequence and avoid shelling
  out to `git` in unit tests; integration tests can use the real
  `gitops.Core.Status` against a `testutil.InitGitRepo` repo.
- Watcher tests must drain the `Updates()` channel with a timeout
  (debounce makes everything async); never `time.Sleep` for
  synchronization.
