# cmd/ao-harness

The shell driver for a running agent test harness (or soak) instance:
boot one, seed it, script its mock providers, wait on its event wire,
read its database and its evidence logs, stop it again. Contract:
[docs/specs/testing-harness.md](../../docs/specs/testing-harness.md) §3.
Harness itself:
[docs/architecture/agent-harness.md](../../docs/architecture/agent-harness.md).

It exists because the harness's whole RPC and event surface was
reachable only from Playwright. An agent debugging a live instance had
to write a WebSocket client first, which meant most of them did not, and
instead guessed from log files.

## What it is not

A pure client. It links `internal/harnessclient` (WS peer plus process
supervisor) and no App code, so it cannot fabricate app state: every
capability here is an RPC the backend already exposes, and every file it
reads is one the backend already writes. That is what keeps the CLI from
becoming a second, divergent way to drive the app.

## Command surface

Global flags work before or after the command name, because an agent
will type either:

| Flag | Meaning |
|---|---|
| `--instance <id\|dataRoot>` | which instance to act on (see resolution below) |
| `--registry-dir <dir>` | override the discovery registry directory (tests) |
| `-o <text\|json>` | output format; text is terse tables, json is stable |

| Command | Purpose |
|---|---|
| `up` | boot a detached instance; `--window --soak --data-dir --binary --mock-provider --dev-assets --keep-home --timeout` |
| `down [--all]` | SIGTERM, then kill after 5s |
| `list` | known instances, pruning rows whose process is gone |
| `info` | identity, URL, and every evidence path |
| `open [--browser]` | print the instance URL |
| `rpc <Method> [json...]` | call any bound method by name with positional JSON |
| `seed -f <spec.json\|->` | apply a `HarnessSeed` spec |
| `reset` | wipe app state without rebooting |
| `threads`, `items --thread <id> [--turn N]` | read the store through the app's own listings |
| `send --thread <id> <text>` | send a message |
| `scenario set\|list\|clear\|validate` | mock scenario rules; `validate` is offline |
| `mock list\|advance\|emit\|exit` | drive registered mock providers |
| `events tail\|await\|count` | the event wire; `--channel`, `--where`, `--timeout` |
| `record start\|stop`, `bundles`, `replay ...` | bundle capture and playback |
| `logs backend\|frontend-errors\|ui-trace [-f] [-n N]` | evidence files |
| `db '<SELECT ...>'` | one read-only statement against the instance database |
| `ui snapshot\|query\|state\|diff` | the attached frontend, through the harness bridge |
| `perf start\|stop\|status\|watch` | the perf meters |
| `bench <workload>` | run a scripted workload with the meters armed and write a report |
| `health [--watch]` | roll one instance's liveness, errors, memory and mocks into a verdict |

### Exit codes

`0` success, `2` wrong invocation, `1` anything the harness or the
filesystem refused. `bench --baseline` and `health` add `3`: the command
ran fine and the ANSWER is bad news (a metric drifted, a concern is red).
A script can tell that from "the harness refused" without parsing prose.

### The frontend commands need a page

`ui`, `perf` and `bench` all ride `HarnessUIQuery`, which is answered by
the harness bridge inside the document. A headless instance answers none
of them, and the error says so and names the two fixes: `make
harness-window`, or open the URL `ao-harness open` prints. `bench` probes
the bridge BEFORE it resets anything, so a caller who forgot the window
gets their instance back untouched.

- `ui snapshot [--pane id] [--settled-ms N] [--save]` prints the rows a
  pane has mounted, with geometry and viewport membership. `--save` (and
  every `ui diff`) writes `<dataDir>/ui-snapshots/last.json`.
- `ui diff [--threshold px]` compares the live page against that file:
  rows mounted and unmounted, rows that entered or left the viewport,
  geometry deltas past the threshold (2px by default, because sub-pixel
  layout noise is not a finding), status and overlay changes.
- `ui query <selector>` and `ui state <name> [json args]` are the
  element and globals query kinds.
- `perf watch` prints one line per backend sample. There is no per-sample
  p95: percentiles come from a whole-run histogram the page keeps, so
  only `perf stop` can answer one. Watch prints the sample's max instead.

### Bench

`bench <workload> [--repeat N] [--sample-ms] [--baseline file] [--out
dir] [--json]`. Each repeat resets the instance, reloads the page, seeds
its own fixture, arms the meters, drives the workload and stops them.

| Workload | Shape |
|---|---|
| `burst-stream` | sustained text-delta flood, chunked partial writes |
| `giant-turn` | one turn producing 225 items (tool pairs plus text) |
| `subagent-fanout` | three bounded async subagents streaming at once |
| `many-threads` | 30 seeded threads, then a thread-switch storm |

The first three run the `bench-*` scenarios in the library and finish on
`provider:turn_completed` for their thread, which is the first moment the
whole pipeline under test is done: `harness:mock`'s `scenario_done` fires
when the MOCK stopped writing, upstream of parse, triage, persist and
render. `many-threads` drives switches by emitting
`notification:activated`, the channel an OS-notification click rides, so
each switch runs the production `openThreadInPane` path. It does not
exercise the sidebar row itself (hit-testing, hover).

Reports land in `<dataDir>/bench/<workload>-<timestamp>.json` and double
as baselines: `--baseline` reads either that file's `aggregate` map (its
p50 becomes the reference, under a default 25% budget) or a hand-written
`{"metrics": {"frames.p95Ms": {"max": 20}}}` budget, and an explicit
budget wins over a derived reference. Drift exits 3. Without `--baseline`
nothing is compared, so a bench is never a gate by accident.

### Health

`health [--watch] [--interval 30s]` rolls up process liveness and uptime,
new `frontend-errors.jsonl` lines, ui-trace oracle triggers, new backend
stderr, the process tree's RSS, database size, mock liveness, replay
state and any armed perf run. Red is a dead process, a new renderer
FAULT, or a panic in new stderr; oracle triggers and plain error lines
are warn. Warn exits 0 on purpose: a rollup that failed on every stderr
warning would be ignored within a day.

`frontend-errors.jsonl` holds two different things, so the scan splits
them. A fault is an application error nothing caught, and is red. A
notice is the engine talking through `window.onerror` with no stack, and
the only member so far is "ResizeObserver loop completed with undelivered
notifications", which a heavy stream produces routinely. Notices are
counted and reported as warn rather than filtered away: they mean layout
work outran a frame, which this timeline cares about, but nothing threw.

Every FILE concern is since-last-check, through
`<dataDir>/health-cursor.json`. The cursor carries each file's size as
well as its offset, which is how a rotation is detected: uitrace rotates
at a size cap, and a reader that kept its offset would read past the end
of the new file. `--watch` appends timestamped lines, one per section,
with no clear-screen, so a long watch greps like any other log.

## Instance resolution

Every command that is not `up` resolves a target first, in this order:

1. `--instance`, read as an instance id when a registry row carries it
   and as a data root otherwise.
2. Exactly one LIVE registry row.
3. This worktree's default data root, `instanceinfo.DefaultDataRoot()`,
   which is the same value `make harness` and the backend's own flag
   default compute.

Two live instances is an error listing the candidates, never a guess.
The commands that act (`reset`, `down`, `mock exit`) are destructive
enough that picking the wrong one silently is worse than making the
caller type eight hex characters.

Attaching then reads `<dataRoot>/agent-overflow/harness-instance.json`
for the token. A registry row deliberately carries no token, so the CLI
must be able to open the data root either way.

## The registry contract

Rows live in `<user cache dir>/agent-overflow/harness-instances/<id>.json`
and are written by the instance itself (`internal/harness/instanceinfo`).
They are discovery state about a DATA ROOT, not about a process, which
is what decides when `list` may delete one:

- The row's pid is alive: keep it, obviously.
- The pid is dead and the data root's own instance file is missing or
  unreadable: delete on sight. Nothing there claims the root.
- The pid is dead but the instance file names the SAME dead pid: delete.
  That is one killed instance's whole set of leftovers.
- The pid is dead and the instance file names a DIFFERENT pid: keep, and
  list it as stale. A second process is involved and the row is not ours
  to remove.

`up` applies the mirror image before booting: it refuses a data root
whose instance file names a live process (two backends on one SQLite
file is the failure it exists to prevent) and allows a boot over a dead
one (otherwise every crash would need manual cleanup).

## Two guards worth keeping

**`db` is read-only twice over.** The connection is opened
`mode=ro&immutable=0&_pragma=query_only(1)`, and the statement is
checked before it is sent: exactly one statement, first keyword in
SELECT / PRAGMA / EXPLAIN. `WITH` is refused because
`WITH x AS (...) DELETE FROM ...` is valid SQLite whose first keyword
says nothing about what it does. The scan understands SQLite's quoting
and comment forms only well enough to know which semicolons are
separators; it is not a SQL parser and must not grow into one. The
safety comes from the connection, and the check is what turns a
violation into a sentence instead of a driver error.

**`up` detaches.** The instance has to survive the CLI exiting, so the
child gets its own session/process group, stderr goes to
`<dataDir>/logs/backend-stderr.log` (which IS its console, and what
`logs backend` tails), and stdout goes to a sibling file the launcher
polls for the bootstrap line. A pipe would hand the child SIGPIPE the
moment the CLI returned.

## Layout

One file per command family, plus the router. Adding a verb is a row in
`commands()` and a `run*` function.

- `main.go`: package doc, the command table, `Run`, usage.
- `cli.go`: exit codes, the `env` every command takes, permuted flag
  parsing, and the output helpers (`writeRawJSON` re-indents the
  server's own bytes rather than re-marshalling, which would reorder
  objects and turn integers into floats).
- `instance.go`: target resolution, attach, `withClient`.
- `cmd_up.go`, `cmd_lifecycle.go`: boot, down, list, info, open.
- `cmd_rpc.go`: rpc, seed, reset, threads, items, send.
- `cmd_scenario.go`, `cmd_mock.go`: the mock-provider surface.
- `cmd_events.go`, `where.go`: the event wire and its `--where` filter.
- `cmd_replay.go`, `cmd_logs.go`, `cmd_db.go`: bundles, evidence, store.
- `cmd_ui.go`, `cmd_perf.go`: the bridge-backed commands.
- `ui_diff.go`: the typed viewport mirror of
  `frontend/src/lib/harness/snapshot.ts` and the comparison over it. The
  ONE RPC result this CLI types in full, because a field name that
  silently decoded to its zero value would render "nothing moved" about a
  page that moved, which is the failure `ui diff` exists to catch. Keep
  the shapes in step with the bridge.
- `cmd_bench.go`, `bench_drive.go`, `bench_seed.go`, `bench_report.go`:
  the run sequence, what a workload actually does to the app between
  arming and stopping the meters, the fixtures it seeds, and the
  arithmetic (aggregation, baselines) split out so the maths is testable
  without a backend.
- `cmd_health.go`, `health.go`: the rollup and its pure half (cursor, log
  scanners, the verdict-to-exit-code rule), same split for the same
  reason.

## Anti-patterns

- Do NOT import App or transport-server code. If a capability needs
  something the wire does not expose, add the RPC on the `Harness`
  receiver and call it from here.
- Do NOT type an RPC result unless a mistyped field would be silently
  wrong. `HarnessInfo` is typed because every consumer wants a PATH;
  everything else stays `json.RawMessage`, because a CLI that typed
  each result becomes a second copy of the app's wire surface.
- Do NOT let a read command guess between instances. Ambiguity is an
  error with candidates.
- Do NOT add a write door to `db`.

## Testing

`go test ./cmd/ao-harness/` covers the router and flag permutation,
instance-resolution precedence and its ambiguity errors, the registry
prune rule in all three shapes, the `db` statement guard (accepted
reads, refused writes, piggybacked statements, semicolons inside
literals) against a real SQLite file, and the `--where` matcher. It also
covers the three pure surfaces W5 added: the `ui diff` renderer on canned
snapshots, the bench aggregation and baseline arithmetic (both baseline
shapes, both drift directions, the report/baseline round trip), and the
health cursor, log scanners and exit-code rules on canned files. Nothing
here boots a backend; the client's own frame handling is tested against
a fake transport server in `internal/harnessclient`, and the real boot
is `make e2e`'s job (`e2e/tests/harness-bench.spec.ts` drives
`bench burst-stream` as a subprocess against a page-attached harness).
