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
becoming a second, divergent way to drive the app. One named carve-out:
`health` reads the backend's `/proc` tree directly (`procrss.SampleAll`)
— liveness must not depend on the wire of the process being judged. It
is the only sanctioned out-of-band read; anything else goes through an
RPC.

One caveat on `open --browser`: the URL it launches carries the RPC
token, which lands on the browser's argv (`/proc/*/cmdline`) and in
history. The default — printing the URL — does not.

## Command surface

Global flags work before or after the command name, because an agent
will type either:

| Flag | Meaning |
|---|---|
| `--instance <id\|idPrefix\|dataRoot>` | which instance to act on (see resolution below); defaults to `$AO_HARNESS_INSTANCE` |
| `--registry-dir <dir>` | override the discovery registry directory (tests) |
| `-o <text\|json>` | output format; text is terse tables, json is stable |

| Command | Purpose |
|---|---|
| `clone --from <real dataDir>` | build a harness data root from a scrubbed copy of a real app data dir; `--data-dir --force`. Does not boot |
| `up` | boot a detached instance; `--window --soak --autopilot --data-dir --binary --mock-provider --dev-assets --keep-home --timeout` |
| `down [--all]` | SIGTERM, then kill after 5s. On a Windows-launcher instance it then closes the launcher window too, via WSL interop against the `launcherPid` the backend published — after confirming the pid's image name is an agent-overflow launcher (see `launcher_kill.go`). Refusals are one operator note, never a failed command |
| `list` | known instances, pruning rows whose process is gone |
| `info` | identity, URL, and every evidence path |
| `open [--browser]` | print the instance URL |
| `rpc <Method> [json...]` | call any bound method by name with positional JSON; `--list [pattern]` prints what this instance exposes, and a `method_not_found` answers with the near names |
| `seed -f <spec.json\|->` | apply a `HarnessSeed` spec |
| `reset` | wipe app state without rebooting |
| `threads`, `items --thread <sel> [--turn N]` | read the store through the app's own listings; `threads` prints the `#` index the selector uses |
| `send --thread <sel> <text>` | send a message; `--wait [--timeout 30s] [--items]` blocks until that thread's turn completes |
| `scenario set\|list\|show\|clear\|validate\|from-thread` | mock scenario rules; `show` and `validate` are offline (the library is compiled in), and `from-thread` rebuilds a real thread's last turns into a scenario document |
| `mock list\|advance\|emit\|exit` | drive registered mock providers; `mock list` carries the open GATE, and `mock advance [mock-id] [gate]` reports what the release actually did |
| `events tail\|await\|count\|channels` | the event wire; `--channel`, `--where`, `--timeout`, and `await`'s `--since`. `channels` is offline |
| `record start\|stop`, `bundles`, `replay ...` | bundle capture and playback |
| `logs backend\|frontend-errors\|ui-trace [-f] [-n N]` | evidence files |
| `db '<SELECT ...>'` | one read-only statement against the instance database |
| `ui snapshot\|query\|state\|diff\|reload\|open` | the attached frontend, through the harness bridge |
| `perf start\|stop\|status\|watch` | the perf meters |
| `bench <workload>` | run a scripted workload with the meters armed and write a report |
| `profile` | record a CPU profile of one scripted turn (needs a Chromium devtools endpoint) |
| `health [--watch]` | roll one instance's liveness, errors, memory and mocks into a verdict |
| `version` | this CLI's build stamp (`--version` answers the same, before any instance is resolved) |

### Exit codes

`0` success, `2` wrong invocation, `1` anything the harness or the
filesystem refused. `bench --baseline` and `health` add `exitBadNews`
(`3`, defined once in `cli.go`): the command ran fine and the ANSWER is
bad news (a metric drifted, a concern is red).
A script can tell that from "the harness refused" without parsing prose.

Two placements worth knowing. AMBIGUITY is `2`, not `1`: two live
instances, or a `--instance` prefix matching several, means the
invocation was under-specified rather than that anything refused. And a
`bench --baseline` whose run never MEASURED an explicitly budgeted metric
is `3`, not `0` — a gate that could not read the number it was told to
gate is bad news, and a headless run (no page, so no frontend metric)
used to print an empty comparison table and exit 0.

### The frontend commands need a page

`ui`, `perf` and `bench` all ride `HarnessUIQuery`, which is answered by
the harness bridge inside the document. A headless instance answers none
of them, and the error says so and names the two fixes: `make
harness-window`, or open the URL `ao-harness open` prints. `bench` probes
the bridge BEFORE it resets anything, so a caller who forgot the window
gets their instance back untouched.

- `ui snapshot [--pane id] [--settled-ms N] [--text-head N]` prints the
  rows a pane has mounted, with geometry and viewport membership.
  `--save` defaults to TRUE for both `ui snapshot` and `ui diff`: each
  writes `<dataDir>/ui-snapshots/last.json`, so consecutive diffs
  compare against the previous look at the page; pass `--save=false` to
  keep the standing baseline. `ui query` takes `--text-cap N` for the
  same text-truncation control.
- `ui diff [--threshold px]` compares the live page against that file:
  rows mounted and unmounted, rows that entered or left the viewport,
  geometry deltas past the threshold (2px by default, because sub-pixel
  layout noise is not a finding), status and overlay changes.
- `ui query <selector>` and `ui state <name> [json args]` are the
  element and globals query kinds.
- `ui reload` and `ui open --thread <sel>` are the two page actions
  `bench` already performed internally, exposed because they are what a
  human debugging a live instance needs by hand: `reset` leaves the SPA
  holding rows that no longer exist, and a seeded thread is invisible
  until the page is told to open it. `reset` PROBES the bridge and prints
  `note: a page is attached — run 'ao-harness ui reload'` rather than
  reloading on its own: a reset that moved someone's page without being
  asked is a surprise in the middle of a debugging session.
- `ui open --new-pane` opens beside the others instead of in the focused
  pane, and it is a DIFFERENT mechanism from the plain open — deliberately.
  The plain one emits `notification:activated`, the channel an
  OS-notification click rides, so the SPA resolves the thread and calls
  `openThreadInPane` itself: a whole production path exercised with no
  bridge in the loop, which is what `bench many-threads` measures a switch
  with, and it must stay that way. The new-pane door has no event channel
  — `openThreadInNewPane` is reached only in-page (ctrl-click on a sidebar
  row, the thread context menu, a builtin command) — so `--new-pane` asks
  the bridge's `open` query kind to call that same function. Minting a
  pane harness-side instead would put a pane nobody ships on the screen.
- `perf watch` prints one line per backend sample. There is no per-sample
  p95 and no per-sample budget fit: both come from whole-run histograms
  the page keeps, so only `perf stop` can answer one. Watch prints the
  sample's worsts instead — `MAXMS` for the frame gap, `BUSYAVG` /
  `BUSYMAX` for main-thread busy time, dashed out when the window
  measured no tick (zero busy time and no measurement are opposite
  findings).
- `perf start --budgets 6,8,16` sets the busy meter's budgets, and
  `perf stop` reports the share of ticks that fit each. That is the
  question a frame-gap histogram cannot answer at all: under a
  vsync-locked compositor every tick's gap reads ~16.7ms whatever the
  work cost. An entry that does not parse, or is not positive, is
  REFUSED rather than skipped — a shortened budget list is a gate
  quietly not being enforced.
- `perf stop` also prints the run's eight WORST busy ticks, each with the
  moment it started on the page clock and, when the page reported a
  `performance.timeOrigin`, the same instant as a wall clock. The
  histogram says what the distribution was; this says WHERE TO LOOK, which
  is what turns a p99 into a trace range worth opening. It rides the same
  busy block into a bench report's per-run rows and is deliberately absent
  from `aggregate` — a list of timestamps is evidence, not a metric, the
  same rule `--trace`'s call-site ranking follows.

### The event wire

`events await` waits for what happens NEXT. `--since` defaults to `now`,
and that default is the whole correctness of the command: `await` used to
pull the server's whole replay ring and settle on the OLDEST match in it,
and since every invocation is a fresh connection, nothing carried the
client's "a wait consumes its match" rule across one — `events await
--channel provider:turn_completed` returned a turn that finished ten
minutes ago, instantly, forever. Pass `--since <seq>` or `--history` to
reach into the ring on purpose; when history IS asked for the scan runs
NEWEST-first, because a caller reaching back wants the latest occurrence,
not the oldest still retained. `tail` keeps replaying history by default
— a tail is a reader, not an assertion.

`events channels [pattern]` prints the vocabulary, offline: the registry
(`internal/eventchan`) is a compile-time table. `tail`/`await`/`count`
WARN on a channel that is not in it and run anyway — the harness
publishes onto caller-named channels through an explicit escape hatch, so
"not registered" means "the backend does not emit this by itself", not
"this cannot carry traffic". Refusing would break the escape hatch to
catch a typo.

`send --wait` subscribes and PARKS its wait before it calls
`SendMessage`, because a mock can complete the turn inside that round
trip.

### Thread selectors

Every `--thread` (`send`, `items`, `ui open`) takes four spellings, all
resolved against the same listing `threads` prints, in the same order:
the full id, `#N` (the index column that listing carries), `last` (most
recently updated), or a case-insensitive TITLE PREFIX when exactly one
row matches. An ambiguous prefix is an error listing the candidates with
their indices; a miss is an error naming the listing. Resolution happens
BEFORE the command's own RPC — `items --thread garbage` used to print "no
items" and exit 0, which reads as "that thread is empty", the wrong
finding entirely and the one a caller is least likely to double-check.

### The state-clone repro rig

Two verbs answer the same question — "reproduce this on MY state" — from
opposite ends. `clone` copies the DATA (thousands of real threads, real
sizes, real shapes) into a harness root. `scenario from-thread` copies
one thread's BEHAVIOUR back onto the mock wire. Neither one needs the
other, and together they turn "it only janks on my machine" into a
scripted, repeatable run.

**`clone --from <real dataDir> [--data-dir <root>] [--force]`** builds a
harness data root from a copy of a real app data dir and stops. It does
not boot; it prints the `up` line.

The source may be LIVE, so the database is never file-copied. The source
is opened `mode=ro` and snapshotted with `VACUUM INTO`, which produces
one consistent file with the WAL folded in. One measured divergence from
`db`'s DSN: `query_only(1)` REFUSES `VACUUM INTO` outright ("attempt to
write a readonly database"), even though the statement writes only to the
output path, so the snapshot connection carries `mode=ro` alone — which
is the layer that actually enforces read-only anyway, exactly as the `db`
guard's own note says. One honest caveat: a read-only WAL open makes
SQLite create `-shm` / `-wal` sidecars beside the source when they are
absent. No database CONTENT is touched (a test hashes the source file
before and after), and a running app already has both.

Then the TARGET copy — opened read-write, the source never is — is
scrubbed:

- every `threads` row loses its resume handles: `session_ref = NULL`,
  `pending_fork_session_ref = NULL`, `pending_fork_resume_at = ''`, the
  triple the store always writes together.
- `thread_import_state` keeps its rows and loses its identity:
  `source_session_id`, `leaf_uuid`, `source_parent_session_id` are
  emptied.
- `ui_state` is DELETEd wholesale. It is client-scoped restore state, and
  carrying it over is the stale-reference toast storm `HarnessReset`
  already fixed once.

Each scrub statement runs with its table's TRIGGERS dropped and restored
around it, from the DDL `sqlite_master` holds so the restore cannot drift
from the source's schema version. The scrub is an offline neutralization
of a dead copy, and the store's integrity triggers judge it as live
traffic: migration v63's `thread_import_state_unique_source_update`
ABORTs when a second row of one provider reaches the same
`source_session_id`, which is exactly what blanking every row's identity
does on any store with two imports from one provider (found live
2026-08-26, 1811 rows). A trigger whose DDL cannot be read back, or that
fails to recreate, is a loud error — a copy that silently enforces less
than its source is not a clone.

A table missing from an older database is a stderr note and an "absent in
this database" receipt, not a failure. The receipt also carries the
copy's schema version (`MAX(version) FROM migration_versions`) as a
diagnostic: the CLI links no store code so it cannot gate on it, but boot
migrates an older store forward and refuses a newer one loudly, and the
line is what makes that refusal attributable to the clone.

Only `attachments/` comes across besides the database. Deliberately NOT
copied: `settings.json` (harness boot re-points the binaries at the mock
every time anyway), `provider-accounts.json` (its `providerHome` stamp
names the REAL home), `replay/`, `ui-trace/`, `design-workdirs/`,
`logs/`, `account-audit.log`, `usage-backoff.json`,
`harness-instance.json`.

The target runs `up`'s own refusals (config root, real app data dir,
symlinked components) plus three of its own, all BEFORE anything is
created: a root whose instance file names a LIVE pid, a target that is
the source or contains it either way round, and an existing target
database unless `--force`. `--from` accepts either the data dir itself or
its parent. It does NOT apply `db --file`'s real-data-dir refusal — this
verb's contract is the exact opposite, an explicit operator choice to
read real data.

**The privacy rule.** The clone carries real conversation content
VERBATIM. Credentials do not travel (they live under the provider home,
which the harness redirects to `<dataRoot>/home`) and nothing can resume
a real session (every handle above is neutralized), but the threads are
the developer's own. The clone lives in its target root, and it is never
committed anywhere. The command says so in its own output.

**`scenario from-thread --thread <sel> [--turns N] [--out f] [--set]
[--delay-ms 15]`** reads the TARGET INSTANCE's store read-only and
rebuilds its last N turns (default 1) as a scenario document.

Streamed text and thinking are cut into deltas at the RECORDED chunk
boundaries: `payloads.data` is the head and each `payload_chunks` row is
one flushed delta, so one emitted line per piece reproduces the original
stream shape rather than a re-chunking of the final text. Tool pairs keep
their recorded `(turn_index, item_index)` order and their `completion_of`
pairing; a completion with no pairing is an error, not a silent drop.
App-internal kinds (notification, api_retry, compaction, …) are skipped
and COUNTED, and the counts print.

Reads go through `timeline_items` / `timeline_payloads` (the v61 logical
views), not the base tables, so an IMPORTED thread does not rebuild
empty. Every statement filters `thread_id` — `payloads` has been keyed
`(thread_id, id)` since v58 precisely because ids repeat across threads,
and `frontend/scripts/generate-freeze-replay-fixture.mjs` predates that
and still has the bug.

The document replays ASSISTANT work only. User text is never a frame,
because a real `send` is what opens each Turn — so the command finishes
by printing the drive recipe, one `send --thread <id> --wait '<text>'`
per rebuilt turn carrying the recorded prompt.

The Codex half is deliberately PARTIAL. `agentMessage` streaming and the
turn envelope are demonstrated verbatim by the library's `codex-basic`,
so those replay. Reasoning and tool items are REFUSED with a message
naming what is missing: a reasoning item needs its delta method
(`textDelta` vs `summaryTextDelta`, model-class dependent) and its
summary structure; a tool item needs its wire item type
(commandExecution / fileChange / mcpToolCall / …) and that type's own
payload and completion fields. A normalized `tool_name` plus a rendered
result cannot reconstruct any of it, and the repo does not guess provider
behavior — a refusal that says what it lacks is worth more than a
plausible fabricated dialect.

The whole recipe:

```
ao-harness clone --from ~/.local/share/agent-overflow
ao-harness up --data-dir <root the clone printed> --window
ao-harness scenario from-thread --thread last --turns 3 --set
ao-harness send --thread <id> --wait '<the recorded prompt>'
```

### Bench

`bench <workload> [--repeat N] [--sample-ms] [--budgets 6,8,16]
[--baseline file] [--out dir] [--json] [--trace --cdp <endpoint>]`. Each repeat resets the instance,
reloads the page, seeds its own fixture, arms the meters, drives the
workload and stops them.

| Workload | Shape |
|---|---|
| `burst-stream` | sustained text-delta flood, chunked partial writes |
| `giant-turn` | one turn producing 225 items (tool pairs plus text) |
| `subagent-fanout` | three bounded async subagents streaming at once |
| `multi-pane-stream` | three panes side by side, all flooding at once |
| `many-threads` | 30 seeded threads, then a thread-switch storm |

The first four run the `bench-*` scenarios in the library and wait on
`provider:turn_completed` for their thread (one wait per thread for
`multi-pane-stream`), which is the first moment the WIRE half of the
pipeline under test is done: `harness:mock`'s `scenario_done` fires when
the MOCK stopped writing, upstream of parse, triage, persist and render.
`many-threads` drives switches by emitting `notification:activated`, the
channel an OS-notification click rides, so each switch runs the production
`openThreadInPane` path. It does not exercise the sidebar row itself
(hit-testing, hover).

**The measured window does not end at turn completion.** It ends when the
REVEAL QUEUE is empty, which is later — the mock finishes a burst-stream
turn in about a second and the reveal keeps handing text to the reader for
ten or more, so every number taken before this covered the flood and
excluded the half a human spends the most time watching. Every workload
(including `many-threads`, which normally passes on its first reading)
waits the same way, and so does `profile`.

Consequence for old reports: **`duration.ms` in a baseline taken before
this reads shorter.** Comparing across the change shows a large drift on
that metric that is purely the window moving. Re-take the baseline.

The signal is `window.__aoRevealDrain()` — panes, panes still draining,
total live smoothers, engaged reveal gates — polled at 250ms through the
`globals` query kind, and it needs two consecutive empty readings because
a drain empties BETWEEN rows. It is cheap store state deliberately, NOT
the bridge's mutation clock: `perf start`/`stop` force-disarm that
observer precisely so a run measures a renderer without one, and re-arming
it to detect quiet would perturb the experiment being timed. It is also
strictly a read — nothing here may skip, rush or pop the drain.

Three degradations, none of them a failed run, each one operator note: a
page that does not install the probe answers `unavailable`, a bridge whose
whitelist has never heard of it answers with a query error, and a drain
still going after 60s (`benchDrainTimeout`) ends the window anyway naming
what was outstanding. A slow drain is a finding to read in the report, not
a reason to throw away the run that produced it.

`multi-pane-stream` is the one workload with a PREPARE step. Its first
pane opens like everyone else's; the other two go through the bridge's
`open` kind with `newPane` (see `ui open --new-pane` above) BEFORE the
meters and the trace are armed, because mounting three timelines is setup
rather than the thing being measured. Then all three turns are sent back
to back — three panes revealing at once share one main thread, one
style/layout pass and one frame budget, which is the whole point; sending
serially would measure three sequential turns.

Reports land in `<dataDir>/bench/<workload>-<timestamp>.json` and double
as baselines: `--baseline` reads either that file's `aggregate` map (its
p50 becomes the reference, under a default 25% budget) or a hand-written
`{"metrics": {"frames.p95Ms": {"max": 20}}}` budget, and an explicit
budget wins over a derived reference. Drift exits 3. Without `--baseline`
nothing is compared, so a bench is never a gate by accident.

Two arithmetic rules the file's readers depend on. A budget of `0` is
BINDING, not absent: `{"max": 0}` is the strictest thing the file can
say, and reading it as "no opinion" would silently accept every value. A
metric a run could not measure is OMITTED from the aggregate rather than
folded in as zero — that covers a headless run with no frontend half,
the series meters (`domNodes`, `jsHeap`) a given engine may never
sample, and a busy half whose `ticks` is zero (a 0% fit would read as a
catastrophic regression rather than as an unarmed meter). `sampleMs` in
the document is the interval the backend RESOLVED, not the one the flag
asked for, because a default run asks for `0`.

The busy meter's metrics gate like any other: `busy.p50Ms`, `busy.p95Ms`,
`busy.maxMs` (lower is better) and one `busy.fitPct.<budget>ms` per
budget the run carried — HIGHER is better, so its budget is a floor
(`{"busy.fitPct.6ms": {"min": 90}}`). Those fit names are the only part
of the metric vocabulary derived from the REPORTS rather than declared in
`benchMetrics()`, because `--budgets` is a run-time flag; repeats that
disagree contribute the union, each folded over only the repeats that
measured it.

`--trace` adds the one thing the bridge cannot answer: which JS call
sites FORCED layout or style recalculation. It records a Chromium timeline
trace around each repeat — started after the reload, settle and thread
open so the recording covers the WORKLOAD rather than the mount, ended
after the meters stop so draining tens of megabytes over the debugger
socket is not itself main-thread work on the page being measured — and
groups `UpdateLayoutTree` / `Layout` events by the top frame of their
stack. The stack IS the signal, not a heuristic over it: only a
script-triggered invalidation has a JS stack to carry, because the
engine's own end-of-frame pass runs from nothing. Events with no stack are
counted separately as engine-scheduled rather than dropped, so a reader
sees the ratio.

The result lands in the report as `trace` (merged over the repeats, top 15
call sites, with the tail COUNTED rather than silently cut) and per repeat
as `runs[].trace`'s two headline numbers. It is deliberately NOT part of
`aggregate`: a baseline compares numbers a headless run can also produce,
and a call-site ranking is evidence, not a metric. Requires a devtools
endpoint (see Profile below); `--trace` with none is refused BEFORE the
first reset, so a caller who forgot the flag gets their instance back
untouched.

The bench connection narrows its event subscription to the completion
channel its drivers await. It is an instrument: leaving it on the default
all-channel subscription makes the backend serialise every item delta
onto a second socket during the exact window being measured.

### Profile

`profile --thread <sel> --scenario <name> [message] [--out file]
[--cdp <endpoint>] [--settle-ms 2000] [--open-settle-ms 2500]
[--interval-us 100] [--timeout 90s]`.

One scripted turn under the V8 sampling profiler, written out as a
`.cpuprofile` (default `<dataDir>/profiles/profile-<timestamp>.cpuprofile`)
plus a three-way rollup of the sampled time. It attaches the debugger,
settles, opens the thread (the same `ui open` activation), settles again,
arms the profiler at a 100µs interval, installs the scenario, sends the
message, waits for `provider:turn_completed`, waits for the reveal queue
to drain (same rule and same degradations as a bench — the tail a reader
watches is main-thread work like any other, and a profile that stopped at
turn completion attributed none of it), and stops.

It does NOT reload the page, and that is the difference from `bench`. A
reload is how a bench gets a blank slate; here it would profile the mount
instead of the turn, on a document that has not been alive long enough to
hold the state the investigation is about.

The rollup splits sampled time three ways, and the split is the whole
point. FLUSH is Svelte running queued effects (`flush_queued_root_effects`,
`flush_queued_effects`, `process_effects`, `update_effect`,
`update_derived`, `execute_derived`, `update_reaction` anywhere in a
sample's ancestry). MARKING is the write side — `internal_set` /
`mark_reactions` — which fires from ANY state write, including from inside
an effect. So marking is checked FIRST and wins wherever it appears: a
sample inside `mark_reactions` under an effect flush is marking cost, and
folding it into flush would attribute the write side's dirty-walk to the
framework's render pass, which is exactly the confusion the split exists
to prevent. Everything else is `other`.

The named split only works on a build that KEEPS those names: the
embedded production dist is minified, so against a plain harness instance
every sample lands in `other`. The rollup detects that (no named svelte
frames matched, yet `svelte-vendor-*.js` time is present), says so
outright instead of printing a misleading 0%, and always prints a
by-script table — self time per chunk basename, which minification does
not rename. For the flush/marking split itself, profile an instance
serving the dev server (unminified names).

#### Both CDP verbs need a Chromium page

`profile` and `bench --trace` are the only two commands here that do not
go through the harness bridge, because a CPU profile and a timeline trace
are Chromium instruments and no bridge can synthesize them. They need a
DevTools endpoint, named by `--cdp` (a port, `host:port`,
`http://host:port`, or a `ws://` page url) or by `$AO_CDP_URL` /
`$AO_CDP_PORT`.

**A WebKitGTK window serves no DevTools protocol at all.** `make
harness-window` on Linux produces an instance every other command here
drives perfectly and these two cannot touch — the refusal says so, and
names this instance's own port when the registry row knows its mode (the
Windows WebView2 shells publish soak on 9224 and harness on 9225; an
external Chrome or Edge does with `--remote-debugging-port`). An absent
endpoint is exit 2 (under-specified invocation), an unreachable one is
exit 1, and both carry that note.

Target selection never guesses: the page whose URL is on the instance's
own origin wins, a single page is taken when nothing matched, and anything
else is an error listing the candidates with their debugger urls. A
browser with three tabs open must not be profiled at whichever one the
listing put first — the numbers would look plausible and describe the
wrong document.

### Health

`health [--watch] [--interval 30s]` rolls up process liveness and uptime,
new `frontend-errors.jsonl` lines, ui-trace oracle triggers, new backend
stderr, the process tree's RSS, database size, mock liveness, replay
state, any armed perf run, whether a soak's autopilot actually armed, and
embedded-asset freshness (`HarnessInfo.assetsFreshness`: a binary built
against a different `frontend/dist` than the one on disk serves — and
measures — a bundle nobody ships; `stale` and `dev-server` are warn).
Red is a dead process, a new renderer FAULT, a panic in new stderr, or a
`soakAutopilot` the backend reports as `failed:` — a soak whose autopilot
threw looks identical to a healthy idle instance from the outside, so an
hours-long run can sit there measuring nothing. Oracle triggers and plain
error lines are warn. Warn exits 0 on purpose: a rollup that failed on
every stderr warning would be ignored within a day.

There are FOUR statuses, not three. `n/a` is "this concern could not be
evaluated", which is not the same claim as `ok` ("it WAS evaluated and
came back clean") and never worsens the verdict. A ui-oracle section over
zero trace records is the case that forced it: a harness build ships with
`UI_TRACE` unset, so the usual reason there is nothing to read is that
nothing was ever written, and reporting `ok` told a caller the oracles
passed when they never ran. A backend that does not report autopilot
state at all is `n/a` for the same reason.

A data root nothing ever claimed — no registry row AND no instance file,
so the target came from the default-data-root fallback — is its own
verdict, exit 1, naming `ao-harness up`. It used to report a dead process
at exit 3, which reads as "your harness crashed".

`frontend-errors.jsonl` holds two different things, so the scan splits
them. A fault is an application error nothing caught, and is red. A
notice is the engine talking through `window.onerror` with no stack, and
the only member so far is "ResizeObserver loop completed with undelivered
notifications", which a heavy stream produces routinely. Notices are
counted and reported as warn rather than filtered away: they mean layout
work outran a frame, which this timeline cares about, but nothing threw.

Every FILE concern is since-last-check, through
`<dataDir>/health-cursor.json`. The cursor carries two facts beyond the
offset, and each catches a rotation the other cannot. SIZE catches the
file that SHRANK — uitrace rotates at a size cap, and a reader that kept
its offset would read past the end of the new file. IDENTITY (device +
inode, empty where the platform has no cheap answer) catches the file
that was REPLACED and then grew back past the old offset, which looks
like ordinary growth to the size check and would silently skip every line
in between. Failing to WRITE the cursor is a stderr warning, never a
failed command: the rollup is already computed and already correct, and
the cost is one over-report next time. `--watch` appends timestamped
lines, one per section, with no clear-screen, so a long watch greps like
any other log.

## Instance resolution

Every command that is not `up` resolves a target first, in this order:

1. `--instance` (defaulting to `$AO_HARNESS_INSTANCE`), read as a full
   instance id, then as a unique id PREFIX (git-style, four hex
   characters minimum), then as a data root.
2. Exactly one LIVE registry row.
3. Several live rows, one of which is THIS worktree's default data root.
   A developer with a soak in one checkout and a harness in another means
   "mine" every time, and the default root is a derivation rather than a
   guess — the same one `make harness` used to create it.
4. This worktree's default data root, `instanceinfo.DefaultDataRoot()`,
   which is the same value `make harness` and the backend's own flag
   default compute.

Anything still ambiguous is an error listing the candidates — with the
WORKTREE column, which is the only thing that tells two of them apart for
a human — and it exits 2, never a guess. The commands that act (`reset`,
`down`, `mock exit`) are destructive enough that picking the wrong one
silently is worse than making the caller type four hex characters.

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

`down` applies the same rule before it SIGNALS: a row's pid is only
believed when the data root's own instance file names the same pid, and
anything else is refused naming the root. Pruning a row on a bad guess
costs a stale listing; SIGKILLing a recycled pid kills whatever inherited
the number.

`up` applies the mirror image before booting: it refuses a data root
whose instance file names a live process (two backends on one SQLite
file is the failure it exists to prevent) and allows a boot over a dead
one (otherwise every crash would need manual cleanup).

## Two guards worth keeping

**`db` is read-only twice over, and harness-only.** The connection is
opened `mode=ro&immutable=0&_pragma=query_only(1)`, and the statement is
checked before it is sent: exactly one statement, first keyword in
SELECT / PRAGMA / EXPLAIN. `WITH` is refused because
`WITH x AS (...) DELETE FROM ...` is valid SQLite whose first keyword
says nothing about what it does. The scan understands SQLite's quoting
and comment forms only well enough to know which semicolons are
separators; it is not a SQL parser and must not grow into one. The
safety comes from the connection, and the check is what turns a
violation into a sentence instead of a driver error. Note WHICH layer
that is: `PRAGMA` is on the whitelist, so `PRAGMA query_only=0` reaches
the handle and SQLite honours it — mode=ro is what still refuses the
write, and one invocation runs one statement so nothing can loosen the
flag and then use it. Never drop mode=ro on the theory that query_only
covers it.

`--file` is refused when it resolves (through symlinks) inside the app's
real data directory — located through `internal/appdirs`, the same
fallback chain the app itself boots on, so the guard cannot drift from
what it is guarding. A root that cannot be resolved at all refuses the
flag rather than allowing it. Without the flag this command
asks a HARNESS instance where its store is, and that instance already
refused to boot on real app data; `--file` skipped both, so one path
could page an agent through the developer's own threads.

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
- `cmd_up.go`, `cmd_lifecycle.go`: boot, down, list, info, open. `up`
  runs the backend's own refusals (config root, real app data dir,
  symlinked components) BEFORE it creates anything — reimplemented rather
  than imported, because this binary links no App code. The duplication
  is three small checks whose drift costs a worse ERROR MESSAGE; not
  having them cost a directory tree created inside the real config root
  before anyone said no.
- `cmd_rpc.go`: rpc, seed, reset, threads, items, send.
- `cmd_scenario.go`, `cmd_mock.go`: the mock-provider surface.
- `cmd_clone.go`, `cmd_scenario_from_thread.go`, `scenario_synth.go`: the
  state-clone repro rig. `cmd_clone.go` opens on the threat model, so a
  reviewer can check it without reading the body, and writes its scrub as
  literal SQL for the same reason `cmd_up.go` reimplements the boot
  refusals — this binary links no store code. `scenario_synth.go` is the
  pure half (recorded items in, wire frames out, nothing touching a
  database) split out so every framing rule is testable with no instance
  anywhere; `cmd_scenario_from_thread.go` is the command and its
  thread-scoped reads.
- `cmd_events.go`, `where.go`: the event wire, its `--where` filter, and
  `parseSince` — the one place `--since` and `--history` are reconciled,
  so the two flags cannot disagree about which window a wait covers.
- `cmd_replay.go`, `cmd_logs.go`, `cmd_db.go`: bundles, evidence, store.
- `cmd_ui.go`, `cmd_perf.go`: the bridge-backed commands.
- `ui_diff.go`: the typed viewport mirror of
  `frontend/src/lib/harness/snapshot.ts` and the comparison over it. The
  rule for typing a result at all is "does this CLI compare or aggregate
  it": the viewport does, because a field name that silently decoded to
  its zero value would render "nothing moved" about a page that moved,
  which is the failure `ui diff` exists to catch. Everything the CLI only
  prints stays `json.RawMessage`. Keep the shapes in step with the bridge
  — `e2e/tests/harness-bridge.spec.ts` runs this binary against a real
  page and fails when a renamed TS field starts decoding to zero.
- `cmd_bench.go`, `bench_drive.go`, `bench_seed.go`, `bench_report.go`:
  the run sequence, what a workload actually does to the app between
  arming and stopping the meters, the fixtures it seeds, and the
  arithmetic (aggregation, baselines) split out so the maths is testable
  without a backend.
- `cdp.go`, `cmd_profile.go`, `cpuprofile.go`, `bench_trace.go`: the two
  CDP-backed verbs. `cdp.go` is the shared half (endpoint resolution from
  `--cdp` / the env, page attachment, and the one sentence about which
  engines serve the protocol); `cpuprofile.go` and `bench_trace.go` hold
  the pure document arithmetic — the flush/marking rollup and the
  forced-layout grouping — split out for the same reason `bench_report.go`
  is, so the maths is testable with no browser anywhere. The wire itself
  is `internal/cdpclient`.
- `threadsel.go`: the `--thread` selector and its resolution, with
  `pickThread` split out pure so every spelling and every refusal is
  testable without a backend.
- `channels.go`: the hand-kept roll call of `internal/eventchan`'s
  constants. It names the CONSTANTS, so a rename fails the compile here;
  a channel ADDED there and never listed is caught by the AST cross-check
  in `cmd_events_test.go`. Go cannot enumerate a package's constants at
  runtime, which is why this file exists at all.
- `version.go`: the build stamp (`-X main.version` from the Makefile's
  `harness-build`), plus the attach-time warning when this CLI and the
  instance it just reached were built from different trees.
- `cmd_health.go`, `health.go`: the rollup and its pure half (cursor, log
  scanners, the verdict-to-exit-code rule), same split for the same
  reason. The cursor's file identity comes from
  `harnessclient.FileIdentity`, shared with `FollowFile` so the two
  rotation detectors cannot disagree.

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
instance-resolution precedence (named id, unique id prefix, the only live
row, this worktree's own) and its ambiguity errors, the registry prune
rule in all three shapes, the `db` statement guard (accepted reads,
refused writes, piggybacked statements, semicolons inside literals)
against a real SQLite file plus its cell-truncation rules, the `--where`
matcher, and the two selector surfaces: `pickThread`'s four spellings
with its ambiguity and miss refusals, and `advanceArgs`' positional
grammar.

It also covers the pure surfaces the later waves added: the `ui diff`
renderer on canned snapshots (plus the snapshot FILE's own round trip and
version check); the bench aggregation and baseline arithmetic (both
baseline shapes, both drift directions, explicit zero budgets, unsampled
series, the report/baseline round trip) and the gate verdict over it; the
health cursor, log scanners and exit-code rules on canned files; and
`parseSince`'s window table. The busy half of a report gets the same
scrutiny for the same reason: both directions of its gate (a percentile
ceiling and a fit-percentage FLOOR — the one place an inverted
`LowerIsBetter` would report a regression as an improvement), the budget
spelling a baseline key has to match, the union rule across repeats,
`--budgets` parsing, and the unmeasured rule that keeps a `ticks: 0` run
out of the aggregate rather than scoring it 0% fit. Three of those are verdicts a caller gates
on and a wrong answer would be BELIEVED: a never-booted root, an `n/a`
that must not read as a pass, and a `--baseline` run whose budgeted
metric was never measured.

`bench_workload_test.go` covers the workload TABLE and the fixtures behind
it, which is where a mistake is otherwise only visible by running a bench
for real: every workload has a summary, a seed and a drive; only
`multi-pane-stream` prepares (a workload that grew a prepare by accident
would move mount cost back inside the measured window) and it names a
scenario; its seed gives every pane its own thread with the completed turn
`ListThreads` needs to show a row at all; and `revealDrain.empty()` needs
BOTH counters clear, because a pane whose last smoother is gone can still
be holding the gate for the row about to start. The worst-tick strip is
tested where it is rendered: its ordering, its dash for a page that
reported no time origin, its absence for a run that carried none, its
exclusion from `aggregate`, and its survival of the report round trip —
a field the CLI decoded but never re-marshalled would vanish from every
bench document silently.

The two CDP verbs are tested at their pure halves and their refusals,
never against a browser: the profile rollup over a hand-built
`.cpuprofile` (including the case the split exists for — an `internal_set`
NESTED inside an effect flush, which must count as marking — plus unknown
sample ids, negative deltas, short delta arrays and a looped tree that
must not hang), the forced-layout grouping over a canned trace (both
container shapes, a malformed `args` that must not fail the document,
stackless events counted as engine-scheduled, the merge across repeats and
its counted truncation), and the endpoint refusals: `bench --trace` with
no endpoint exits 2 without driving anything, and `profile` refuses in
order (thread, scenario, endpoint) before it attaches.

Two cross-checks earn their keep here rather than in a review:
`TestKnownChannelsCoversTheEventChannelRegistry` AST-parses
`internal/eventchan` and diffs it against `channels.go`, and the
launcher-kill tests pin that unparseable tasklist output is an ERROR
rather than "the process is gone" — the two used to collapse into one
answer and leave a live launcher's window on the desktop with nothing
said.

The state-clone rig is tested entirely on SYNTHETIC data — a `t.TempDir`
source dir holding a real SQLite file with the store's real column names,
never a copy of anyone's app — because a test that reached for real data
to prove it handles real data safely would have already lost. `clone`'s
tests pin each scrub (the session-ref triple, import identity neutralized
without dropping the row, `ui_state` emptied), the copy set in both
directions (attachments come, nothing else does), every refusal, and the
one property the whole verb rests on: the SOURCE database is byte-identical
after a clone of a database a writer still holds open. The fixture
carries migration v63's uniqueness triggers VERBATIM plus two
same-provider import rows — the shape the real store aborts a naive
scrub on — and the trigger test asserts both halves of the
stash/restore: the triggers exist in the copy afterwards, and the
restored copy still ABORTS a duplicate claim (an inert restored row
would weaken the schema in silence). `from-thread` is
tested at its pure halves — the frame sequence for a three-chunk text
item, thinking's own block vocabulary, tool pairing and its unpaired
refusal, per-turn id namespacing, skipped-kind counting, a document that
survives the real `scenario.Parse`, the Codex leg's demonstrated shape and
its explicit refusal of the rest — and at its reads: turn windowing, the
head-then-chunks assembly, and two threads sharing a payload id that must
not cross.

Finally, the refusals whose wrong answer would be destructive rather than
merely wrong: `down` on a pid the data root does not confirm, `db --file`
aimed at real app data, and `up`'s own three — one of which asserts that
NOTHING was created inside the root it refused. Nothing
here boots a backend; the client's own frame handling is tested against
a fake transport server in `internal/harnessclient`, and the real boot
is `make e2e`'s job — `e2e/tests/harness-bench.spec.ts` drives
`bench burst-stream` as a subprocess against a page-attached harness, and
`e2e/tests/harness-bridge.spec.ts` runs `ui snapshot` the same way and
reads its TEXT rendering, which is the only place the hand-kept
`ui_diff.go` mirror is checked against the TS snapshot it copies.
