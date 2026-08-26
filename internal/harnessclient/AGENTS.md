# internal/harnessclient/

The Go client for a running agent test harness or soak instance: how to
find one, how to start one, how to speak its wire, and how to read the
evidence files it leaves behind. Twin of `e2e/src/harness.ts` — the same
contract from Go, so `cmd/ao-harness` and any Go test share one
implementation of it instead of two that drift.

Harness itself:
[docs/architecture/agent-harness.md](../../docs/architecture/agent-harness.md).

## What it is not

A second backend. This package links no App code and no transport SERVER
code. Everything it touches is what a foreign process can already
observe: a JSON line on the child's stdout, a 0600 file in the data dir,
one WebSocket, and the log files the instance writes. That is the whole
reason `cmd/ao-harness` cannot fabricate app state.

## Layout

- `bootstrap.go` — the package doc, the `Bootstrap` payload (the stdout
  line AND `<dataDir>/harness-instance.json`, one shape for both),
  `BootstrapPrefix`, and the readers that find an instance from a data
  root or from a captured stdout line.
- `info.go` — the one RPC result this package TYPES (`HarnessInfo`),
  because every consumer of it wants a path. `SoakAutopilot` rides along
  as an optional string (`armed` / `failed: <reason>` / `off`); an older
  backend omits it, and a reader must treat absent as "not judged".
- `frames.go` — the wire frames. See the restatement rule below.
- `client.go` — `Dial` / `DialURL`, the read loop, `Call` / `CallRaw`,
  `Subscribe` / `Replay`, `Close`, and the read limit.
- `events.go` — the event log: `WaitForEvent`, `WaitForEventOpts`,
  `Await`, `Count`, `Events`, `Clear`, `Listen`, and `dispatch`, which is
  where the log and waiter rules below actually live.
- `launch.go`, `spawn_unix.go`, `spawn_windows.go` — detached boot: own
  session/process group, stderr to a file that IS the child's console,
  stdout to a sibling file polled for the bootstrap line. A pipe would
  hand the child SIGPIPE the moment the parent returned.
- `process.go` — liveness and signalling for a pid. `TerminateProcess`
  CONFIRMS its kill (see below).
- `tail.go` — `TailFile` (last N lines) and `FollowFile` (`-f`).
- `fileident_unix.go`, `fileident_other.go` — `FileIdentity(FileInfo)`,
  the one platform-split call here. Unix answers device+inode from
  `FileInfo.Sys()`; everywhere else answers empty, and every caller falls
  back to its size heuristic alone. `cmd/ao-harness`'s health cursor uses
  the same function, which is why it is exported.

## The frame restatement is deliberate

`frames.go` mirrors `internal/transport/frame.go` by hand. Importing the
real structs would link the server into every CLI and test binary, which
is exactly the coupling this package exists to avoid.

What keeps a hand copy honest is the drift guard: `fakebackend_test.go`
is a transport-shaped server that decodes what this client SENDS through
`transport.ClientFrame` and answers with `transport.ServerFrame`. So the
tests exercise this package's private frames against the real ones on
every run, and a field that is renamed on one side fails the other. If
you add a field here, add the case that crosses it through the fake
backend — a restatement with no traffic over it is a copy that will rot.

Only the fields a client reads or writes are declared. The server
tolerates omitted optional fields, and restating the full struct would
buy a maintenance burden with no assertion behind it.

## The event log

One in-memory log of pushed frames, and every assertion reads it.

- **`WaitForEvent` CONSUMES its match.** Waiting twice for the same shape
  observes two distinct occurrences rather than the first one twice —
  multi-turn assertions depend on it, and the TS client behaves
  identically. Consumption is a property of the LOG entry, not of the
  waiter, which is what makes "already arrived" and "arrives later" the
  same rule: history is scanned first, so a fast backend cannot win the
  race against the caller.
- **The predicate sees the whole `Event`, not just its payload.** A
  replay gap marker is an event on the channel carrying no data, and it
  must not satisfy a wait for real traffic.
- **The longest-waiting waiter wins.** Waiter keys come off a monotonic
  counter and `dispatch` picks the lowest matching one. Map iteration is
  randomized, so this is enforced rather than inherited: two identical
  concurrent waits are ordered, not a coin flip.
- **`Count` is the absence half.** A wait can only prove something
  happened; `Count` is how a test proves something did not. Twin of
  `countEvents` in `e2e/src/harness.ts` — keep the two in step.
- **The log sheds in CHUNKS at its cap** (`defaultEventLogCap`, 10k,
  matching the TS client). At the cap the oldest quarter goes in one
  move. Evicting one entry per arrival is an O(cap) memmove on every
  subsequent event — quadratic over a sustained stream, on the read loop,
  under the mutex. The cost is that a `Count` taken just after a shed
  sees less history than the cap implies, which the log's contract
  already allows: it is an assertion surface over recent traffic, not a
  history store. A run long enough to overflow it is a soak, and a soak
  asserts on the evidence files.
- **A wait that TIMES OUT consumes nothing, and a wait that times out
  after the event arrived still returns it.** `dispatch` marks the log
  entry consumed and hands the event to the waiter's channel while holding
  the mutex, so a deadline that only deleted the waiter would destroy the
  sole copy: consumed in the log, unread in a channel nobody holds. The
  timeout path deletes AND drains in one critical section, so "dispatch
  got there first" and "the deadline got there first" is a decision rather
  than a race with two losers.
- **`Await` parks the wait BEFORE the caller causes the event.** Register,
  then send — a mock can complete a turn inside the `SendMessage` round
  trip, and a wait started afterwards would miss it. Every `Await` must
  end in `Wait` or `Close`, or its waiter lives as long as the connection.
- **`WaitForEventOpts` tunes WHICH occurrence settles a wait.** `Newest`
  scans retained history newest-first (right for a one-shot CLI that just
  pulled a replay ring, wrong for a client stepping through a stream),
  `MinSeq` floors it at a server sequence, and `SkipHistory` parks without
  looking at history at all. The zero value is `WaitForEvent`'s behaviour.
- **`Listen` callbacks run ON the read loop.** They must not call back
  into the client: a `Call` would wait for a response the blocked read
  loop can never deliver. Print, count, or hand the event to a channel.

## Following a file

`FollowFile` polls (`followPollInterval`, a package var so tests can
shrink it — production never writes it) and emits COMPLETE LINES ONLY.
Two bounds make that safe against a fast writer: each poll reads at most
`followReadCap`, and the trailing partial line is carried in a buffer
with the read offset advanced PAST it. Leaving the offset before the
fragment instead — the obvious implementation — re-reads and re-scans the
same bytes every tick until the writer finishes the line.

A rotation is detected two ways, and each catches one the other cannot.
SIZE catches the file that SHRANK (a truncating rotate). IDENTITY
(`FileIdentity`, device+inode) catches the file that was REPLACED — a new
inode renamed over the name — and then grew back PAST the old offset,
which looks like ordinary growth to the size check and would silently
resume mid-record inside a file the follower has never read. Where
identity is empty (a platform with no cheap answer) the size heuristic is
the whole answer. Either way the carried fragment is dropped with the
file it belonged to, because its newline is never coming.

## Confirming a kill

`TerminateProcess` sends SIGTERM, waits out `grace`, then SIGKILLs — and
then POLLS until the pid is actually gone, failing with `pid N survived
SIGKILL after <window>` if it is not. A successful `Kill()` is a signal
DELIVERED, not a process removed: an uninterruptible sleep (a wedged 9p
read under WSL, a stalled NFS mount) outlives it, and a caller that
printed "stopped" over a still-running backend is how two of them end up
on one SQLite file. `KillConfirmWindow` is a package var so a test does
not have to spend it.

## Anti-patterns

- Do NOT import `internal/transport` outside `_test.go`. The drift guard
  is a test-only dependency on purpose.
- Do NOT let `WaitForEvent` stop consuming. Every multi-turn assertion in
  both clients is written against that behaviour.
- Do NOT let a wait's abandonment path delete its waiter without draining
  it. That is the lost-event shape; both halves belong in one critical
  section.
- Do NOT grow the log into a history store. If an assertion needs more
  than the recent window, it wants the replay ring or an evidence file.
- Do NOT type an RPC result unless a mistyped field would be silently
  wrong. `HarnessInfo` earns it; the rest stays `json.RawMessage`.

## Testing

`go test ./internal/harnessclient/` runs everything against
`fakeBackend`, an httptest server speaking the real transport frames — no
App, no boot. It covers RPC round trips and error envelopes, batch and
ping frames, the wait/consume/count rules, the chunked shed, waiter
ordering, subscribe/replay reaching the server, close semantics, the
detached launch, and the tail/follow cases (including a rotate-and-regrow
that only identity can see). Two of them are about the timeout path
specifically — a wait that times out consumes nothing, and an event
delivered in the race window is still returned — because a lost event
there is invisible until an assertion hangs somewhere else. The
survived-SIGKILL report is exercised by stubbing the liveness probe: no
cooperating child can be asked to outlive SIGKILL on demand. The real
boot is `make e2e`'s job.
