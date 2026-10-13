# Agent workflow tools

Status: implemented 2026-10-13 (D77). The behavior of the system these tools
drive is [workflows-system.md](workflows-system.md) §5; this is the tool
contract. The tool descriptions in `internal/workflowtools/tools.go` are what
an agent reads and carry the same schemas.

## Approach

An in-process MCP server, `ao-workflow-tools`, on the shared
`internal/threadmcp` transport, the same way as
[agent thread tools](agent-thread-tools.md): loopback HTTP and a per-session
capability URL that session start issues and session end revokes. A call on a
URL whose session has ended is refused with `workflow_session_inactive`.

Session start registers it for every Claude, claude-tui and Codex thread in a
project:

| Provider | Registration | Guide |
|---|---|---|
| Claude | `--mcp-config` entry plus `--allowedTools mcp__ao-workflow-tools__*` | the server's `instructions` |
| claude-tui | the same two flags; it hosts no other app server (`docs/decisions.md`) | the server's `instructions` |
| Codex | `mcp_servers` entry with tool approval `approve` | developer instructions on thread start |

A thread with no project has no workflow tools. The server is always on for
a project session: its row in the thread's MCP menu has no toggle.

Each call runs the bound workflow methods in process with the session's
caller scope on the context, so the row-level rules of §5 (which runs a phase
may act on, surface-and-skip for a phase's `run_start`, binding an
interactive start to its thread) are the methods' own.

## Who sees which tools

An interactive session sees every tool. A phase session sees the tools its
phase's `grants:` admit and the grant-free ones; `tools/list` returns exactly
that set, and the guide names only those tools.

| Tool | Admitted by |
|---|---|
| `workflow_list`, `workflow_validate`, `workflow_schema`, `memory_list` | no grant |
| `workflow_new`, `memory_add` | no grant; withheld from read-only phases |
| `run_start`, `run_control`, `run_amend`, `run_guide` | `start-run` |
| `run_status`, `run_watch`, `run_output`, `run_inspect`, `run_narrative` | `start-run` or `introspect` |
| `run_list` | `introspect` |
| `run_resolve` | `resolve` |

Memory and `workflow_list`/`workflow_validate`/`workflow_schema` are
grant-free because a phase could already reach them through the offline CLI
and the memory verb, so nothing a phase can do widened. Grant-free tools never
widen what a phase can do, and a read-only phase keeps exactly the envelope
memory channel it had. A phase is read-only unless its access is `write`: the
access its run's frozen definition declares for the phase, or for the fan-out
unit the session runs, never the session's runtime mode, which a user can
change on a live thread.

Grants take effect on read-only phases: the author declared them and
validation accepted them, and that a read-only session could not reach the old
CLI was a mechanism accident, not a rule.

A call outside the caller's tools is refused by name, never narrowed:

| Code | When | Message names |
|---|---|---|
| `workflow_grant_required` | the phase holds none of the tool's grants | the grants that would admit it, and that the workflow author adds them to the phase's `grants:` |
| `workflow_read_only` | `memory_add` or `workflow_new` from a read-only phase | the read-only access, and the envelope `memory` field |
| `workflow_invalid_request` | an unknown argument, a missing required one, a value out of range | the argument |
| `workflow_call_failed` | the app refused the call | the app's own reason |

Refusals are MCP tool errors (`isError: true`) whose text starts with
`[<code>]`.

## Tools

Arguments are snake_case. Every schema is closed (`additionalProperties:
false`). A result is the app's own document, camelCase, unchanged.

### Authoring

- `workflow_list()`: `{workflows: [{id, name, scope, path, shadowsShared,
  bindingStatus, findings}], notes?}`. `notes` names directories discovery
  skipped.
- `workflow_validate(id | path)`: exactly one. `path` is absolute; a file
  under a project's workflow directory validates under that project, any
  other under this one. Returns `{findings, reports?, bindingStatus}`.
- `workflow_schema()`: the definition JSON schema.
- `workflow_new(starter, id, scope)`: `scope` is `project` or `shared`.
  Returns `{created, validation, note?}`. Never overwrites a file.

### Runs

- `run_start(workflow_id, scope?, goal?, seeds?, base_branch?, step_mode?,
  wait_seconds?, notify?)`: returns `{start: {itemId, workflowId,
  workflowScope, state, skipped?, boundThreadId?, bindingWarning?}}`, plus the
  wait fields below when it waits. A phase's repeated start with the same
  arguments returns the first run with `skipped: true`. Every argument,
  `wait_seconds` included, is checked before the run starts; once it has
  started the answer always carries `start`, and a run that cannot be read
  back afterwards answers `outcome: "unknown"` with `readError` rather than an
  error, so the caller never starts it twice.
- `run_status(run_id, wait_seconds?, notify?)`: the run status document.
- `run_watch(run_id, cursor?, tree?, wait_seconds?)`: `wait_seconds` defaults
  to 60. Returns `{itemId, cursor, transitions: [{seq, at, itemId, phaseId?,
  attempt?, from?, to, reason?, cause?, resting}], run: {itemId, workflowId?,
  goal?, state, reason?, phaseId?, resting, repair?}, gap?}` as soon as the
  run moves, rests, or the wait ends. A call with no cursor answers at once.
  `repair` is the sentence a wake would end on for the same park.
- `run_output(run_id)`: `{itemId, state, reason?, resting, outputs,
  artifacts}`.
- `run_inspect(run_id, phase_id?, attempt?)`.
- `run_narrative(run_id, phase_id, attempt?, unit_id?)`: `{itemId, phaseId,
  attempt, unitId?, unitAttempt?, path, present, bytes?, truncated?,
  content?}`.
- `run_list(active_only?)`: run status documents without the single-run
  fields (`seeds`, `failedUnits`, `phases`, `budget`, `pendingGuidance`).

### Control

- `run_control(run_id, action, ...)`: `action` is one of `pause`, `resume`,
  `cancel`, `rerun`, `soft_stop`, `retry_unit`, `retry_failed_units`. Each
  takes only its own fields:

  | Action | Fields |
  |---|---|
  | `resume` | `phase_id`, `refresh_def`, `at` (RFC 3339 or `+36h`; not with the others) |
  | `rerun` | `guidance`, `refresh_def` |
  | `soft_stop` | `clear` |
  | `retry_unit` | `unit_id` (required), `note` |
  | `retry_failed_units` | `note` |

  Returns the run status document after the action; a scheduled resume
  returns `{armedAt, run}`. An action that took effect when the run cannot
  be read back returns `{itemId, action, done: true, armedAt?, readError}`.
- `run_amend(run_id, seeds)`: `{itemId, names, seeds, phaseId?, effect,
  appliesNote, callerNote?}`.
- `run_guide(run_id, text)`: `{itemId, pending, maxPending, by, state,
  reason?, phaseId?, deliversNote, callerNote?, quarantineNote?}`.
- `run_resolve(run_id, decision, note?, answer?)`: `decision` is `approve`,
  `reject` or `answer`; `answer` requires `answer`. Returns the run status
  document, or `{itemId, action, done: true, readError}` as `run_control`
  does.

### Memory

- `memory_add(kind, text, files?, run_id?)`: `kind` is `pattern`, `warning`,
  `learning` or `handoff`. A phase defaults to its own run; an interactive
  session names one. Returns `{itemId, rootId, kind, wave, path}`.
- `memory_list(kind?, run_id?)`: `{itemId, rootId, path, notes, total,
  skipped}`.

### The run status document

`{itemId, workflowId, goal, state, reason?, currentPhaseId?,
currentPhaseOrdinal?, phaseCount?, parentItemId?, resting, startedAt?,
endedAt?}`, and on the single-run reads:

- `seeds`: the run's frozen input object.
- `failedUnits[]`: `{unitId, unitAttempt, note?}`, the ids `retry_unit` takes,
  on a run resting `needs-human(unit-failed)`.
- `phases[]`: `{phaseId, attempt, status, provider?, model?, effort?, cause?,
  session?, decision?, decisionTarget?, exhaustedLoops?}`. `cause` is the
  engine's park diagnosis; `session` is `"continued"` for an attempt that
  continued an earlier attempt's session. `outputs` and `outputOverflow` are
  filled by `run_inspect` alone.
- `budget`: `{kind, ceilingTokens|ceilingUsd|ceilingMillis, spentTokens,
  spentUsd, elapsedMillis, percent, estimated, unpricedRows?, exhausted,
  rootItemId?}`, absent for a run under no ceiling.
- `pendingGuidance`: how many `run_guide` entries wait for the next fresh
  phase entry.

`run_inspect` returns `{run: <run status document>, worktreePath?, branch?,
baseBranch?, children: [{itemId, workflowId?, goal?, state, reason?,
parentPhaseId?, parentUnitId?, parentAttempt?}], guidance?: [{text, at,
ageSeconds, by, byRun?}], phase?: {phaseId, attempt, status, provider?,
model?, effort?, cause?, outputs?, decision?, decisionTarget?,
exhaustedLoops?, units: [{unitId, kind, status, unitAttempt, note?, branch?,
worktreePath?, threadId?}]}}`. `phase` is present with `phase_id`.

## Waiting

`run_start` and `run_status` take `wait_seconds`, at most 900
(`threadtools.MaxWaitSeconds`), the same ceiling as the thread tools. A
waiting call watches the run without polling and answers with `outcome`:

- `resting`: the run came to rest within the wait;
- `running`: no wait was asked for and the run is working;
- `backgrounded`: the wait ended with the run still working;
- `unknown`: `run_start` only, the run started and could not be read back
  (`readError` says why).

The transport keeps a long call alive, so a wait never times out the
provider's tool call.

## Hearing back

A run's rest reaches a thread only through its binding (§5, D17): a run an
interactive thread starts is bound to that thread, and the bound thread is
woken whenever the run comes to rest. No tool call changes a binding.

A wait that returns the rest is the delivery, as a thread tool's wait is:
that rest does not also wake the thread. While an interactive call waits, the
run's wakes to the caller's thread are held
(`internal/workflowapp/wake_hold.go`). `run_start` takes the hold before it
binds the run, so no rest can slip past it. A rest that arrives while the
hold stands is kept back.

The hold is released once the call's response has left the transport
(`threadmcp.AfterResponse`), on the wake queue behind any wake job already
queued, so such a job still finds it. A call that returned the run at rest
names the rest it read: the watch cursor it read the run at, and the state
and reason it saw. That rest counts as delivered only if the response was
written and the run has not moved since: the watch ring holds no transition
of the run after that cursor, and the stored state and reason are the ones
returned. A delivered rest is recorded as the thread's last delivered wake,
and the wake-coalescing rule suppresses the same rest arriving after the
call. Otherwise, when the call backgrounded, failed, was cancelled, its
response never reached the client, or the run moved on after the read, a
rest the hold kept back wakes the thread as it would have, unless the run
moved after that rest's wake was queued, in which case that move's own wakes
settle it.
The next rest, after the run is resumed, rerun or otherwise acted on, wakes
the thread as usual.

Limits:

- A hold is process memory: a crash between the rest and the call returning
  loses that one wake, as a crash between a rest and its wake always could.
  A call that ends during shutdown releases before the store closes, but the
  app takes no new messages by then, so the kept-back wake is reported as
  undelivered on the run instead.
- A rest whose wake job ran before `run_start` bound the run found no binding
  and is never woken; a `run_start` that waits still returns it.
- The engine writes a transition before it records it in the ring. A rest
  written before the call's store read but recorded after the cursor the call
  read reads as a move, so it is returned and also wakes the thread once.

A backgrounded wait, or any call with `notify: true` on a run not yet resting,
carries `notify: {armed, detail}`. When the binding cannot be read, the status
still stands and `detail` says so:

| Case | `armed` | `detail` |
|---|---|---|
| bound to the caller's thread | true | the rest arrives here as a message; no need to poll |
| unbound | false | this run is not bound to your thread; poll `run_status` again or bind it from the overlay |
| bound to another thread | false | names that thread; poll `run_status` again |
| a phase session | false | phases receive no wakes; call `run_status` again |

## Server instructions

The server's guide is built per caller from the tools it may call, so it never
names a tool the session lacks: the start-and-wait pattern, the repair map
from park reason to call, and, for a read-only phase, that campaign memory
goes in the envelope's `memory` field. Claude reads it from the MCP
handshake; Codex receives it with the app guide in its developer
instructions. The app guide names the server among the app's tools.

## Repair sentences

A wake, `run_watch`'s `repair`, the `/workflow` composer block and the app's
own notes name calls the way an agent makes them, for example
`` `run_control(run_id: "<id>", action: "resume", phase_id: "<phase-id>")` ``.
`internal/workflow/wake` checks every call it names against this server's
tool table: the tool exists, each argument is in its schema, and each
`action` or `decision` is one it accepts.
