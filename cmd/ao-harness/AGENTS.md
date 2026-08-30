# cmd/ao-harness

The shell driver for a running agent test harness (or soak) instance:
boot one, seed it, script its mock providers, wait on its event wire,
read its database and its evidence logs, stop it again.

- **Command surface, flags, subcommands**: `ao-harness help`,
  `ao-harness <group> -h`, and the generated reference
  [docs/references/ao-harness.md](../../docs/references/ao-harness.md)
  (`go generate ./cmd/ao-harness`, which runs the undocumented
  `--generate-docs <path>` mode over the descriptor tree `help` prints).
  `rpc --list [pattern]` prints what one live instance exposes. Keep no
  hand-written command table in this file: the last one drifted five
  commands and a deleted source file behind `commands()`.
- **Mechanism and rationale** for bench workloads, CPU profiling, the
  health rollup, the state-clone repro rig, the typed monitor edge, and
  the frontend bridge:
  [agent-harness.md](../../docs/architecture/agent-harness.md)
  § Driving an instance from a shell. Contract:
  [testing-harness.md](../../docs/specs/testing-harness.md) §3. Soak
  preset: [soak-rig.md](../../docs/architecture/soak-rig.md).

## What it is not

A pure client. It links `internal/harnessclient` (WS peer plus process
supervisor) and no App code, so it cannot fabricate app state: every
capability is an RPC the backend already exposes, and every file it reads
is one the backend already writes. One sanctioned out-of-band read:
`health` samples the backend's `/proc` tree directly
(`procrss.SampleAll`), because liveness must not depend on the wire of
the process being judged. Anything else goes through an RPC.

`open --browser` puts the RPC token on the browser's argv
(`/proc/*/cmdline`) and in history. The default, printing the URL, does
not.

## Rules this binary enforces

**Nothing here may reach the developer's real running app or real
provider homes.** `up` refuses a data root resolving to the OS config
root or the real app data dir, and refuses either as a symlink, because
an isolated boot seeds and wipes those directories wholesale
(`refuseUnsafeDataRoot`, reimplemented rather than imported since this
binary links no App code). `db --file` refuses a path resolving through
symlinks inside the real data dir, located through `internal/appdirs` so
the guard cannot drift from what it guards; an unresolvable root refuses
the flag rather than allowing it. `compare prepare` has its own refusals
(`internal/compare/AGENTS.md`). Two carve-outs, both operator-typed and
both loud: `clone --from <real dataDir>` reads real data by definition,
and `up --keep-home` leaves the real `$HOME` visible to child processes
while backend provider state stays in the harness home. Provider
isolation for harness and soak alike is
[soak-rig.md](../../docs/architecture/soak-rig.md) § Provider isolation.

**Every launch is OOM-safe by construction, never by hope.** `up`
reserves host capacity, installs a hard per-instance memory boundary
before exec (600 MiB default, `--memory-limit-bytes` within host
capacity), and arms a detached watchdog before it reports success. A
launch that cannot install a boundary fails instead of running unbounded.
Any new workload, run adapter, or launch path inherits this or does not
ship. Platform matrix:
[testing-harness.md](../../docs/specs/testing-harness.md). Related:
`bench` resets and mutates the instance it borrows, then leaves it
running, while `run --plan` is the disposable entrypoint and owns an
absent or empty `dataRoot`.

**A soak is this binary with a preset armed, not a second mode.**
`--soak` only selects the launcher-shaped bootstrap contract;
`--autopilot` is what makes it a soak, which is why the registry `mode`
follows `--autopilot` rather than the shell. `up --soak` does not start
the Windows launcher; `make soak` does.

## Memory governor and watchdog

Three layers answering three questions. Do not collapse them.

- `internal/harness/governor` is host-wide bookkeeping: a cross-process
  capacity reservation under an OS file lock, so harnesses started from
  several worktrees cannot overcommit one host. It never signals an
  application.
- `internal/harness/containment` is the OS boundary on one instance
  (cgroup v2, Job Object, or inherited `RLIMIT_DATA`). This is the OOM
  protection, and it fails closed.
- `watchdog.go` re-execs this binary in its undocumented `--watchdog`
  mode: a detached process sampling the lease owner's tree every 100ms
  that, on a ceiling or host-floor crossing, writes
  `logs/harness-watchdog.json`, calls `HarnessShutdown` over the
  authenticated wire, and only then falls back to an identity-checked
  tree kill. It is not a PID-only killer. `up` waits for its
  `harness-watchdog-ready.json` handshake and rolls the launch back if it
  never arms.

Instruments that must not run unbounded attest first
(`requireActiveHarnessBoundary`): the watchdog named by
`harness-watchdog-state.json` has to still be the process holding the
exact live lease, because a stale state file is not evidence anything is
armed.

## Exit codes

`0` success, `2` wrong invocation, `1` anything the harness or the
filesystem refused. `bench --baseline` and `health` add `exitBadNews`
(`3`, defined once in `cli.go`): the command ran fine and the ANSWER is
bad news, so a script tells that from "the harness refused" without
parsing prose. Ambiguity is `2`, not `1`: under-specified, not refused. A
`bench --baseline` whose run never MEASURED an explicitly budgeted metric
is `3`, not `0`, because a gate that could not read its number is bad
news; a headless run used to print an empty table and exit 0.

## Instance resolution

Every command that is not `up` resolves a target first, in this order:

1. `--instance` (defaulting to `$AO_HARNESS_INSTANCE`), read as a full
   instance id, then as a unique id PREFIX (four hex characters minimum),
   then as a data root.
2. Exactly one LIVE registry row.
3. Several live rows, one of which is THIS worktree's default data root.
   A developer with a soak in one checkout and a harness in another means
   "mine" every time.
4. This worktree's default data root, `instanceinfo.DefaultDataRoot()`,
   the same value `make harness` and the backend's flag default compute.

Anything still ambiguous is an error listing the candidates with their
WORKTREE column, and it exits 2, never a guess. `reset`, `down` and
`mock exit` are destructive enough that picking the wrong one silently is
worse than making the caller type four hex characters.

Attaching then reads `<dataRoot>/agent-overflow/harness-instance.json`
for the token; a registry row deliberately carries none. An authenticated
transport connection is the attach-path liveness authority, because a
native Windows CLI driving a launcher-hosted WSL backend cannot resolve
the Linux PID. `down`, row pruning, and every other lifecycle path still
require same-namespace PID evidence before signalling or deleting.

## The registry contract

Rows live in `<user cache dir>/agent-overflow/harness-instances/<id>.json`,
written by the instance itself (`internal/harness/instanceinfo`). They are
discovery state about a DATA ROOT, not about a process, which decides when
`list` may delete one:

- Row's pid alive: keep.
- Pid dead, data root's own instance file missing or unreadable: delete.
  Nothing there claims the root.
- Pid dead, instance file names the SAME dead pid: delete. That is one
  killed instance's whole set of leftovers.
- Pid dead, instance file names a DIFFERENT pid: keep and list as stale.
  A second process is involved and the row is not ours to remove.
- Either side names a different PID namespace: keep. A WSL pid means
  nothing to a Windows CLI and the reverse.

`down` applies the same rule before it SIGNALS: pruning on a bad guess
costs a stale listing, but SIGKILLing a recycled pid kills whatever
inherited the number. `up` applies the mirror image, refusing a root
whose instance file names a live process and allowing a boot over a dead
one.

## Guards worth keeping

**`db` is read-only twice over, and harness-only.** The connection is
opened `mode=ro&immutable=0&_pragma=query_only(1)`, and the statement is
checked before it is sent: exactly one statement, first keyword in
SELECT / PRAGMA / EXPLAIN. `WITH` is refused because
`WITH x AS (...) DELETE FROM ...` is valid SQLite whose first keyword
says nothing about what it does. The scan finds statement separators and
nothing more; it is not a SQL parser and must not grow into one.
`PRAGMA` is whitelisted, so `PRAGMA query_only=0` reaches the handle and
SQLite honours it. `mode=ro` is what still refuses the write, and one
invocation runs one statement so nothing can loosen the flag and then use
it. Never drop `mode=ro` on the theory that `query_only` covers it.

**`up` detaches.** The instance has to survive the CLI exiting, so the
child gets its own session/process group, stderr goes to
`<dataDir>/logs/backend-stderr.log` (its console, and what `logs backend`
tails), and stdout goes to a sibling file polled for the bootstrap line.
A pipe would hand the child SIGPIPE the moment the CLI returned.

**`postmortem` never attaches.** It is deliberately independent of
`instance.go`'s attach path: it reads a STOPPED evidence root, and
opening a wire would make the answer time-dependent and risk talking to
the wrong process.

**`events await` waits for what happens NEXT.** `--since` defaults to
`now`, and that default is the whole correctness of the command: `await`
used to settle on the oldest match in the replay ring and return a turn
that finished ten minutes ago, instantly, forever. `--since <seq>` or
`--history` reaches back on purpose, and then the scan runs NEWEST-first.
`tail` replays history by default: a tail is a reader, not an assertion.
`tail`/`await`/`count` WARN on a channel absent from
`internal/eventchan` and run anyway, because the harness publishes onto
caller-named channels through an explicit escape hatch. `send --wait`
parks its wait before calling `SendMessage`, because a mock can complete
the turn inside that round trip.

**Thread selectors resolve before the command's own RPC.** Every
`--thread` takes the full id, `#N` (the index `threads` prints), `last`,
or a unique case-insensitive title prefix. `items --thread garbage` used
to print "no items" and exit 0, which reads as "that thread is empty":
the wrong finding, and the one a caller is least likely to double-check.

**`ui`, `perf`, `monitor` and `bench` need an attached page**: they ride
`HarnessUIQuery` through the harness bridge in the document. `bench`
probes the bridge BEFORE it resets anything, so a caller who forgot the
window gets their instance back untouched. `profile` and `bench --trace`
instead need a Chromium DevTools endpoint and refuse with that stated:
WebKitGTK serves no CDP at all.

## Anti-patterns

- Do NOT import App or transport-server code. If a capability needs
  something the wire does not expose, add the RPC on the `Harness`
  receiver and call it from here.
- Do NOT type an RPC result unless a mistyped field would be silently
  wrong. `HarnessInfo` is typed because every consumer wants a PATH;
  `ui_diff.go` is typed because the CLI compares it. Everything the CLI
  only prints stays `json.RawMessage`.
- Do NOT let a read command guess between instances. Ambiguity is an
  error with candidates.
- Do NOT add a write door to `db`.
- Do NOT let an instrument perturb what it measures. `bench` narrows its
  subscription to the completion channel it awaits, and its drain probe
  is a read that may never skip, rush or pop the reveal queue.

## Testing

`go test ./cmd/ao-harness/` covers the pure halves and the refusals;
nothing here boots a backend. The real boot is `make e2e`'s job:
`e2e/tests/harness-bench.spec.ts` runs `bench burst-stream` as a
subprocess, and `harness-bridge.spec.ts` runs `ui snapshot` and reads its
text rendering, the only place `ui_diff.go`'s hand-kept mirror of
`frontend/src/lib/harness/snapshot.ts` is checked against the TS.

**Size a fixture to the shape that breaks, not to the smallest thing that
compiles.** `clone`'s scrub passed against a one-row
`thread_import_state` fixture and aborted on the real store's 1811 rows,
because migration v63's uniqueness trigger only fires when a SECOND row
of the same provider reaches the same `source_session_id` (found live
2026-08-26). The fixture now carries the v63 triggers verbatim plus two
same-provider rows and asserts the restored copy still ABORTS a duplicate
claim: an inert restored trigger weakens the schema in silence. The clone
rig is tested on synthetic data only, never a copy of anyone's app.

Two cross-checks earn their keep here rather than in review:
`TestKnownChannelsCoversTheEventChannelRegistry` AST-parses
`internal/eventchan` and diffs it against `channels.go` (Go cannot
enumerate a package's constants at runtime, which is why that roll call
exists), and the launcher-kill tests pin unparseable tasklist output as
an ERROR rather than "the process is gone", which used to leave a live
launcher's window on the desktop with nothing said.
