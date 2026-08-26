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

`ui`, `perf`, `bench` and `health` are not here yet: they are the
frontend bridge's RPCs and land with it.

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
literals) against a real SQLite file, and the `--where` matcher. Nothing
here boots a backend; the client's own frame handling is tested against
a fake transport server in `internal/harnessclient`, and the real boot
is `make e2e`'s job.
