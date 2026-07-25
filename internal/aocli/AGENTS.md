# internal/aocli/

Command routing and presentation for the `ao` binary. Keep provider,
persistence, and app lifecycle concerns out of this package. Command functions
accept arguments and writers directly so tests do not spawn subprocesses.

The binary has two halves:

- **Offline** (`ao workflow …`) — scope discovery, definition validation,
  listing, scaffolding, and the embedded authoring schema. Needs no running
  app and no credential.
- **Execution** (`ao run …`, `ao notes …`, `ao schedule …`) — one HTTP POST per
  invocation against a running Agent Overflow, authenticated with the scoped
  credential the app injected into the calling agent's session (spec §5, D15).

Workflow definition behavior belongs to `internal/workflow/def`; this package
discovers scopes, loads project profiles for binding checks, copies embedded
starter sources, calls the workflow APIs, and renders CLI results. Starter
content and embedding belong to `internal/workflow/starters`.

## The AO_* session contract

`session.go` owns these names — the reader of the contract declares them so the
writer (`sessionProcessEnv` in `app_session.go`, the one place a provider
subprocess's environment is assembled) cannot drift from it.

| Variable | Set for | Meaning |
|---|---|---|
| `AO_ENDPOINT` | every app-started session | the app's loopback base URL, no token, no path |
| `AO_TOKEN` | every app-started session | scoped credential, valid exactly as long as the session |
| `AO_THREAD_ID` | every app-started session | the thread the session belongs to |
| `AO_RUN_ID` | workflow phase / unit sessions | the run this session is a phase of |
| `AO_PHASE_ID` | workflow phase / unit sessions | the phase (a unit carries its phase's id) |

Rules the package enforces:

- Both `AO_ENDPOINT` and `AO_TOKEN` absent means "not inside a session" — a
  distinct, typed message, because that is fixed by running the command
  somewhere else rather than by changing permissions.
- Exactly one of them present is a broken injection, reported as such rather
  than left to surface as an authorization failure.
- A 401 from the route means the credential was revoked, which for a scoped
  token means the session that owned it ended. The CLI says that in those
  words; there is no retry and no re-mint.
- A session with no credential (no transport server, no project, an
  unresolvable phase) gets no AO_* at all. Partial authority is never injected.

## Wire

One `POST {AO_ENDPOINT}/rpc` per invocation, `Authorization: Bearer $AO_TOKEN`,
body and response are `transport.ClientFrame` / `transport.ServerFrame` — this
package imports those types rather than restating them, so the CLI and the
server cannot disagree about the wire. Method NAME only: numeric method ids
exist for generated bindings, and a CLI has none.

Waiting is polling, not subscribing (`waitForRun`): 500 ms, then x1.5, capped at
5 s, `--timeout` default 30 m. A process that makes one call and exits has no
business owning a WebSocket with a replay ring behind it.

`--json` prints the app's own result document verbatim. Human rendering decodes
only the fields it prints into narrow local structs, so the CLI never becomes a
second definition of what a run status looks like.

## Exit codes

Binary-wide, offline and execution alike:

| Code | Meaning |
|---|---|
| 0 | success, including a surface-and-skip replay (the effect the caller asked for exists) |
| 1 | the answer was "no": validation findings, or a `--wait` whose run rested somewhere other than `done` |
| 2 | usage error, or an operational failure (no session, refused, backend unreachable) |

## Command tree

```
ao [--config-root <path>] <command>
  workflow new|validate|list|schema            offline
  run start <workflow-id> [--scope|--goal|--seed k=v|--base-branch|--step|--wait|--timeout|--json]
  run status|wait|output <run-id> [--json] [--timeout]
  run list [--active] [--json]
  run pause|cancel <run-id> [--json]
  run resume <run-id> [--phase <id>] [--json]
  run rerun <run-id> [--guidance <text>] [--json]
  run retry-unit <run-id> <unit-id> [--note <text>] [--json]
  notes get|set <automation-id> [--file <path>] [--json]
  schedule <workflow-id> --cron <expr> [--name|--scope|--seed k=v|--json]
```

Every usage string lives in `usage.go` so adding a subcommand without
documenting it is an obvious omission.

`--seed k=v` parses the value as JSON when it parses, and treats it as a string
otherwise: `--seed count=3` seeds a number, `--seed name=alice` seeds a string,
with no shell quote round trip. A repeated key is an error, not a silent
overwrite.

Flags may appear before, after, or between positionals (`parsePermuted`): Go's
`flag` package stops at the first non-flag token, which would make
`ao run start flow --wait` read `--wait` as a second workflow id. A literal `--`
still ends flag parsing.

## Grants and refusals

The CLI does not evaluate grants — `internal/transport` authorizes the method
against the token's scope, and the bound methods enforce which rows that scope
may touch. What this package owns is that both refusals reach the caller
intact: the typed `grant_required` message naming the missing grant, and the
row-level refusal naming the phase that may act only on the runs it started.

## `/workflow` composer context

`composer.go` is the pure renderer behind the `/workflow` composer command: it
takes a resolved `ComposerContext` (project name, workflow source directories,
available workflows, active runs, whether the thread has a live session) and
returns the text block injected into the conversation. Lists are bounded
(`MaxComposerWorkflows`, `MaxComposerRuns`) and truncation is stated in the
block, never silent. The app-side RPC that resolves the live data is
`WorkflowComposerContext` (`app_workflow_composer.go`); the split exists so the
block format is unit-testable without a database.

## Files

| File | Responsibility |
|---|---|
| `run.go` | Root command routing, config-root discovery, offline workflow commands, exit codes. |
| `usage.go` | Every usage string. |
| `session.go` | The AO_* contract, session resolution, and the scoped HTTP RPC client. |
| `exec.go` | Execution-command skeleton: seed parsing, permuted flag parsing, JSON/human rendering. |
| `exec_run.go` | `ao run …`. |
| `exec_automation.go` | `ao notes …` and `ao schedule`. |
| `composer.go` | The `/workflow` block renderer. |
| `workflow_files.go`, `workflow_new.go`, `profile.go` | Definition discovery, scaffolding, profile binding checks. |
