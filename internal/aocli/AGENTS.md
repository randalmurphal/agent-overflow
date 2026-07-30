# internal/aocli/

Command routing and presentation for the CLI. Keep provider, persistence, and
app lifecycle concerns out of this package. Command functions accept arguments
and writers directly so tests do not spawn subprocesses.

**There is no separate CLI binary (D30).** The CLI *is* the app binary,
dispatched by verb: `agent-overflow run start <id>`, `agent-overflow workflow
list`. `main.go` matches `os.Args[1]` against `Commands()` — this package's
dispatch table, exported so no second copy of the verb set can rot — and hands
the whole argv to `Run`. Agent sessions find the binary because boot publishes
a canonical-name symlink at `<configDir>/bin/agent-overflow` and
`sessionProcessEnv` prepends that directory to every session's PATH. Every
user-facing string here therefore says `agent-overflow`, never `ao`; the AO_*
environment variable names are an internal contract and keep their prefix.

The CLI has two halves:

- **Offline** (`agent-overflow workflow …`) — scope discovery, definition
  validation, listing, scaffolding, and the embedded authoring schema. Needs no
  running app and no credential.
- **Execution** (`agent-overflow run …`, `agent-overflow notes …`,
  `agent-overflow schedule …`) — one HTTP POST per invocation against a running
  Agent Overflow, authenticated with the scoped credential the app injected
  into the calling agent's session (spec §5, D15).

Workflow definition behavior belongs to `internal/workflow/def`; this package
discovers scopes, loads project profiles for binding checks, copies embedded
starter sources, calls the workflow APIs, and renders CLI results. Starter
content and embedding belong to `internal/workflow/starters`.

## The AO_* session contract

`session.go` owns these names — the reader of the contract declares them so the
writer cannot drift from it. The writer is `mintAOCredential`
(`app_ao_session.go`), which builds the map; `sessionProcessEnv`
(`app_session.go`) is the one place it is merged into a provider subprocess's
environment.

| Variable | Set for | Meaning |
|---|---|---|
| `AO_ENDPOINT` | every app-started session | the app's loopback base URL, no token, no path |
| `AO_TOKEN` | every app-started session | scoped credential, valid exactly as long as the session |
| `AO_THREAD_ID` | every app-started session | the thread the session belongs to |
| `AO_PROJECT` | every app-started session with a project | the project's **slug** — what `--project` takes |
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
- `AO_PROJECT` is the **offline** half's only input. The execution commands scope
  themselves from the token, but `workflow list` / `workflow validate --id`
  resolve project definitions from a directory named by slug and have no app to
  ask — so `workflowProjectScope` defaults their scope from it, and an explicit
  `--project` always wins. An empty answer therefore means neither input was
  present, which is what lets an empty listing say why. A malformed value is
  reported as `AO_PROJECT: invalid project slug …`, because the fix is not
  something the caller typed.
- `workflow new` deliberately does **not** inherit it. `--project` is that
  command's write destination, not a filter: inheriting it would silently
  redirect where files land and leave no way to scaffold into the shared scope
  from inside a session. Shared scope is always included by the read commands, so
  a shared scaffold is still listed and validated by the next command run.

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
agent-overflow [--config-root <path>] <command>
  workflow new|validate|list|schema            offline
  run start <workflow-id> [--scope|--goal|--seed k=v|--base-branch|--step|--wait|--timeout|--json]
  run status|wait|output <run-id> [--json] [--timeout]
  run list [--active] [--json]
  run pause|cancel <run-id> [--json]
  run soft-stop <run-id> [--clear] [--json]
  run resume <run-id> [--phase <id>] [--json]
  run rerun <run-id> [--guidance <text>] [--json]
  run retry-unit <run-id> <unit-id> [--note <text>] [--json]
  run retry-failed-units <run-id> [--note <text>] [--json]
  notes get|set <automation-id> [--file <path>] [--json]
  schedule <workflow-id> --cron <expr> [--name|--scope|--seed k=v|--json]
```

Every usage string lives in `usage.go` so adding a subcommand without
documenting it is an obvious omission. `run soft-stop`'s help states the one
thing its exit code cannot: a workflow with no call edge has no boundary to
stop at, so the request is accepted and simply never fires (D36). A verb that
succeeds and then does nothing has to say so where the caller is already
looking.

The human lines carry what the next verb needs (D38): `runView.line()` renders
`parent=<run-id>` so a campaign's flat `run list` shows its tree, and
`failed-units=<ids>` on `run status` so `run retry-unit`'s second argument is
readable from the CLI. Failed units are resolved on the single-run read only —
a list would pay one unit query per row. A `run list` with no rows prints `No
runs in this project.` rather than nothing, because a blank answer reads as a
command that did not work; `--json` still prints the app's own `[]`.

`workflow list` follows the same rule and adds the one cause a blank answer has:
with no rows it prints `No workflows are configured here.`, plus — when no
project scope was resolved from either input — `Project workflows need --project
<slug>, or a session that sets AO_PROJECT.` A hint that cannot apply is worse
than none, so the second line appears only when the scope really is missing.

Both `workflow list` and a failed `workflow validate --id` also report
**skipped directories** (`def.SkippedDirs`): discovery is flat, so a
hand-authored `<id>/workflow.yaml` resolves to nothing at all, and "not found"
for an id whose file is visibly on disk is otherwise unexplainable. The note is
`note: <path> is a directory and was skipped — a workflow is a flat <id>.yaml
beside its <id>-*.md prompts`. It goes to **stderr** in both modes, including
`--json`: it describes the state of the directory rather than a row of the
requested result, so the JSON document stays exactly the list and a machine
reader still cannot lose the information. `validate --id` carries the same notes
inside the not-found error, where the caller is already looking.

`--seed k=v` parses the value as JSON when it parses, and treats it as a string
otherwise: `--seed count=3` seeds a number, `--seed name=alice` seeds a string,
with no shell quote round trip. A repeated key is an error, not a silent
overwrite.

Flags may appear before, after, or between positionals (`parsePermuted`): Go's
`flag` package stops at the first non-flag token, which would make
`agent-overflow run start flow --wait` read `--wait` as a second workflow id. A
literal `--` still ends flag parsing.

## Grants and refusals

The CLI does not evaluate grants — `internal/transport` authorizes the method
against the token's scope, and the bound methods enforce which rows that scope
may touch. What this package owns is that both refusals reach the caller
intact: the typed `grant_required` message naming the missing grant, and the
row-level refusal naming the phase that may act only on the runs it started.

## `/workflow` composer context

`composer.go` is the pure renderer behind the `/workflow` composer command: it
takes a resolved `ComposerContext` (project name and **slug**, workflow source
directories, available workflows, active runs, whether the thread has a live
session, whether boot published the command on PATH) and returns the text block
injected
into the conversation. Lists are bounded (`MaxComposerWorkflows`,
`MaxComposerRuns`) and truncation is stated in the block, never silent. A
`CommandOnPath: false` block says so outright — it is the one place an agent
reads before typing the command, so a boot that could not publish the symlink
must not leave "command not found" as the first news of it. The project-scope
line names the slug `--project` takes (`--project <slug>` when there is none),
because the offline commands cannot infer it and a reader who has only the block
must still be able to write the flag. The app-side
resolver that produces the live data is
`workflowComposerBlock` (`app_workflow_composer.go`); the split exists so the
block format is unit-testable without a database. That resolver is NOT a bound
method — the block never reaches the frontend. The composer holds only the
literal word `/workflow`, and the send path appends the block to the
provider-bound payload (D31, `app_composer_commands.go`).

The block also carries the **reason→verb repair map** (`composerRepair`, D38):
which park reasons a CLI verb repairs, and — just as load-bearing — that gates
and questions are decided by a human in the app. Both tables render through
`writeComposerRows`, which pads the left column in runes at render time, so
editing a row cannot leave the block ragged.

## Files

| File | Responsibility |
|---|---|
| `run.go` | Root dispatch table (`Commands` / `IsCommand` / `Run`), offline workflow commands, exit codes, output helpers. |
| `workflow_scopes.go` | Scope discovery and resolution: config-root, source directories, `ResolveConfigured`, call resolvers, listing. |
| `usage.go` | Every usage string. |
| `session.go` | The AO_* contract, session resolution, and the scoped HTTP RPC client. |
| `exec.go` | Execution-command skeleton: seed parsing, permuted flag parsing, JSON/human rendering. |
| `exec_run.go` | `agent-overflow run …`. |
| `exec_automation.go` | `agent-overflow notes …` and `agent-overflow schedule`. |
| `composer.go` | The `/workflow` block renderer. |
| `workflow_files.go`, `workflow_new.go`, `profile.go` | Definition discovery, scaffolding, profile binding checks. |
