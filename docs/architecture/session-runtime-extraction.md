# Session runtime extraction plan

Status: implemented. `internal/sessionruntime.Manager` now owns the complete
transaction boundary described below; `App.mu` and the compatibility maps were
deleted in the same cut. The pre-cut audit remains here because it documents
why the registry must never be split back into independently locked slices.

## Atomic invariants

`sessionManager.put`, `take`, `unregister`, `takeIdle`, and
`snapshotAndClear` mutate the live session map in the same critical section as
the scoped `ao` token registry and Claude live-config cleanup. A session token
must become resolvable exactly when its process enters the registry and become
unresolvable exactly when that process leaves. Pending/degraded live-config
state keyed by the session token must disappear in that same removal.

`threadIDsForProviderOrStarting` reads `sessions` and `startingSessions` under
one lock. Session registration precedes start-state removal, so a settings
reconcile sees a thread in at least one map throughout the handoff. Moving
either map alone creates a gap where a settings save permanently misses the
new process.

The same mutex currently guards reconnect admission, automatic reconnect
attempts, deferred config reconnects, Claude live-config applies/generations,
the coalesced Claude reconcile sweep, prompt-render cache, per-thread prompt
overrides, and the idle/retention sweep start-stop handshakes. Several of those
paths read or mutate a session in the same critical section. A package owning
only `sessions` would therefore require callbacks into `App` while holding its
lock or a second lock with an unprovable ordering. Both are worse than the
current code.

## Required ownership cut

Create one `internal/sessionruntime.Manager` that owns every current `App.mu`
ward:

- live provider sessions and their liveness counters;
- scoped `ao` token authorities;
- in-flight starts, reconnect admission, and auto-reconnect attempts;
- pending config reconnects;
- Claude live-config apply, degradation, generation, and sweep state;
- prompt render cache and per-thread prompt overrides;
- idle/retention sweep admission handles.

The Manager owns its mutex. `App.mu` and compatibility maps must be deleted in
the same change; two authorities may never coexist.

## Narrow API

- `Entry` is a non-wire runtime value containing provider name, session token,
  typed Claude/Codex/Claude-TUI handles, launch options, credential attribution,
  `ao` authority, and liveness. Root constructs it after spawning.
- `Put`, `Take`, `Unregister`, `TakeIdle`, and `SnapshotAndClear` own registry,
  authority, and live-config cleanup atomically.
- `BeginStart` / `FinishStart` and `ThreadIDsForProviderOrStarting` own the
  spawn-to-registry handoff.
- Typed readers (`Claude`, `Codex`, `ProviderSession`, provider/account
  snapshots, MCP snapshots) expose only what each consumer needs. Do not expose
  the map or invent a `Host` interface mirroring `App`.
- `ResolveAOToken` replaces root access to the authority map.
- Token-guarded launch-option and credential mutation stays on the Manager.
- Provider process creation/close, orphan-reaper calls, event routing, store
  transactions, account selection, queue/revert, and every Wails method remain
  at root.

## Migration order

1. Move the private runtime value/liveness types and pure token-guarded mutation
   tests into `internal/sessionruntime`. Root DTOs are unaffected.
2. Move the `ao` authority registry with session `Put`/removal operations and
   convert the AO authority adapter in `app_session_runtime.go` to Manager
   delegation in the same change.
3. Move start-state ownership and the combined session/start snapshot. Convert
   the start coordinator before deleting the root maps.
4. Move the Claude live-config and prompt state whose cleanup is part of
   removal. This is required before the Manager can own the registry lock.
5. Audit the remaining reconnect/config/sweep wards and move them or give them
   proven disjoint locks. Only then replace `App.mu` with the Manager.
6. Keep the existing root Wails methods (`StartSession`, `StopSession`,
   `ReconnectSession`, `SendMessage`, `SendMessageWithOptions`, `InterruptTurn`,
   `SwitchThread`, `AutoResumeThread`) and their comments/signatures exactly;
   they become lifecycle adapters over the Manager and existing spawn/send
   policy.
7. Move registry/start/reaper/live-config unit tests beside the Manager. Retain
   real mock-provider, transport, shutdown ordering, account adoption, and
   queue/revert integration tests at root. Run package and root race suites,
   the no-real-provider guard, and regenerate Wails bindings.

This is an account/session/thread-core wave, not a mechanical relocation of
`app_session_runtime.go`. Starting with the registry alone would split three
load-bearing transactions.
