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
- `paths.go` — `canonicalize` helper so `/tmp` symlinks don't spawn
  duplicate watchers on macOS.

## How it works

1. `Manager.Subscribe(cwd)` canonicalizes the path, fetches the initial
   `GitStatus` via the injected `StatusFn`, and either reuses an
   existing watcher for that cwd or starts a new one.
2. The watcher attaches `rjeczalik/notify` recursively at `cwd/...` and
   feeds events into a per-cwd channel. A trailing-edge 250ms debounce
   coalesces bursts, then re-runs `StatusFn` and broadcasts to
   subscribers.
3. `lastStatus` is compared with `reflect.DeepEqual` — unchanged status
   does not emit, keeping the wire quiet during heavy fs activity that
   doesn't affect the working tree (build outputs, ignored files).
4. If `notify.Watch` fails at install time (most commonly Linux
   inotify watch-limit exhaustion), the watcher transparently falls
   back to polling at `pollFallbackInterval`. Subscribers see the same
   `Updates()` shape; only cadence changes.
5. Subscribers receive a single buffered channel; on overflow the
   newest status supersedes the older one (the run loop drains the
   pending value before sending).

## Responsibility boundary

- What BELONGS here:
  - Watching a workspace path and turning fs activity into deduplicated
    `GitStatus` updates.
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
