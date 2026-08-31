# internal/orphanreaper/

macOS-only safety net so provider subprocess groups never outlive an
ungraceful app death. Linux has `PR_SET_PDEATHSIG` (see
`internal/provider/process_linux.go`) and Windows uses the launcher's
Win32 Job Object (`internal/wsllauncher`); macOS has neither kernel
primitive for arbitrary children, so we reconstruct it in userspace.

## Why this exists

The provider package spawns each Claude/Codex process in its own process
group (`Setpgid`). On a clean shutdown the app signals that group and the
subprocess (plus its subagents/MCP children) dies. But if the app dies
*ungracefully* (panic, `SIGKILL`, crash), no app code runs, the kernel
reparents the providers to `launchd`, and they linger (~288 MB RSS each;
the Claude CLI ignores stdin EOF). This package closes that gap.

## Two layers (defense in depth)

- **Live sidecar** (`reaper.go`, `client.go`). The app re-execs itself as
  `__reap` (the `Subcommand()` token, routed at the top of `main()`),
  handing the child the read end of a control pipe on fd 3. The app sends
  `watch <pgid>` / `release <pgid>` (`protocol.go`) as sessions start and
  stop. When the app dies by *any* means the kernel closes its write end,
  the sidecar reads EOF, and it `SIGTERM`→`SIGKILL`s every still-watched
  group. This is the userspace `Pdeathsig`.
- **Startup sweep** (`registry.go`, `sweep.go`). A durable JSON registry
  records `{uuid, pid, pgid, create_unix}` per spawn. On the next launch
  `Sweep` kills any recorded group still alive, start-time-matched
  (PID-reuse safe), and reparented to init, the backstop for when the
  sidecar *also* died (both `SIGKILL`ed, power loss).

## Key invariants

- **Kill by process-group id, never by parent.** `kill(-pgid)` needs only
  same-uid permission, so neither layer has to be the provider's parent.
  The app keeps spawning providers exactly as before (`kill_unix.go`).
- **One writer, one reader on the control pipe.** The app holds the write
  end; the sidecar holds the read end (fd 3). Go marks the write end
  close-on-exec, so it never leaks into provider subprocesses. That's
  what makes EOF a reliable death signal. Don't dup the write end into
  anything that outlives the app.
- **Release only after a *successful* Close.** `internal/app/app_orphan_reaper.go`'s
  release fires from `closeProviderSession` only when Close returns nil;
  an abandoned/timed-out close keeps the watch so a still-alive subprocess
  is still reaped if the app then dies.
- **Sweep before any session registers.** `startOrphanReaper` runs `Sweep`
  then clears the registry before the reaper is even spawned, so the
  load→kill→clear can't race a fresh `Add`.
- **pgid ≤ 1 is always refused** (caller's own group / init) in both the
  protocol parser and `killGroup`.

## Lifecycle wiring lives in `internal/app`

`internal/app/app_orphan_reaper.go` owns `startOrphanReaper` / `stopOrphanReaper` /
`watchSessionProcess` / `releaseSessionProcess`, gated on
`runtime.GOOS == "darwin"`. The `Client` methods and those helpers are
nil-safe, so non-macOS builds and tests call them unconditionally with no
sidecar running. This package stays a pure mechanism; the app composes it.

## Testing

- Pure logic (`protocol`, `registry`, `sweep.shouldReap`) has unit tests
  with injected state. `sweepWith` and `run` take an injectable killer so
  tests never signal real groups.
- `reaper_test.go` has a real end-to-end check: `TestMain` re-execs the
  test binary into `RunChild`, watches a real victim process group, and
  closes the pipe to prove EOF reaps it. Keep that green. It's the only
  test exercising the actual kernel EOF→kill path.

## Anti-patterns

- Do NOT make the sidecar relay provider stdio or parent the providers.
  It's a pure signaller; the stdio/parent path stays app↔provider direct.
- Do NOT add heavy startup to the `__reap` branch. It must short-circuit
  before flags/shell-env/Wails so the sidecar stays a tiny pipe reader.
- Do NOT swallow registry errors silently; log them. A failed registry
  write degrades the backstop but the live sidecar still covers the group.
