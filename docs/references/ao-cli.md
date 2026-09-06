# agent-overflow CLI reference

Shapes and surface for the CLI half of the app binary (D30). The rules and the
reasons behind them live in
[internal/aocli/AGENTS.md](../../internal/aocli/AGENTS.md). Flags are
authoritative in `internal/aocli/usage.go`, printed by `agent-overflow help` and
`agent-overflow <group> --help`.

## Command tree

```
agent-overflow <command>
  serve [--listen|--data-dir|--reset-transport-port]
                                               NOT an aocli command: a boot mode routed by
                                               main's entry dispatch, listed in the root usage
                                               because it is a verb a person types
                                               (docs/architecture/serve-mode.md)
  supervise [same boot flags as serve]         also NOT an aocli command, and deliberately NOT in
                                               the root usage: a service manager starts it, never
                                               a person. It owns the backend as a child and is
                                               what makes a serve host updatable
                                               (docs/architecture/serve-mode.md § The supervisor)
  service install [--listen <host:port>] [--binary <path>]
  service update [--binary <path>]
  service uninstall
  service status                               host commands: manage the machine's service
                                               manager, talk to no app, hold no credential.
                                               status exits 1 when it is not running. Refused on
                                               Windows, where the launcher already supervises its
                                               WSL backend (docs/architecture/serve-mode.md).
                                               install writes ExecStart=<binary> supervise, not
                                               serve; update stages a binary as a new version and
                                               restarts the unit, and is the only way to replace
                                               the supervisor itself
  workflow new|validate|list|schema            offline (each takes --config-root; put it after
                                               the subcommand: aocli's root FlagSet would accept
                                               a leading one, but the binary's entry router only
                                               recognises a leading verb, so a flag-first
                                               invocation never reaches these commands)
  run start <workflow-id> [--scope|--goal|--seed k=v|--base-branch|--step|--wait|--timeout|--json]
  run status|wait|output <run-id> [--json] [--timeout]
  run watch <run-id> [--tree] [--timeout <dur>] [--json]
  run inspect <run-id> [--phase <id> [--attempt <n>]] [--json]
  run narrative <run-id> --phase <id> [--attempt <n>] [--unit <id>] [--json]
  run list [--active] [--json]
  run pause|cancel <run-id> [--json]
  run soft-stop <run-id> [--clear] [--json]
  run resume <run-id> [--phase <id>] [--refresh-def] [--at <when>] [--json]
  run rerun <run-id> [--guidance <text>] [--refresh-def] [--json]
  run retry-unit <run-id> <unit-id> [--note <text>] [--json]
  run retry-failed-units <run-id> [--note <text>] [--json]
  run resolve <run-id> --approve|--reject [--note <text>] [--json]
  run answer <run-id> <text> [--json]
  run amend <run-id> --seed k=v [--seed k2=v2]... [--json]
  run guide <run-id> "<text>" [--json]
  memory add --kind <kind> "<note>" [--file <path>]... [--run <id>] [--json]
  memory list [--kind <kind>] [--run <id>] [--json]
  notes get|set <automation-id> [--file <path>] [--json]
  schedule <workflow-id> --cron <expr> [--name|--scope|--seed k=v|--json]
```

## `--json` documents

`--json` forwards the app's own result document verbatim, so these shapes are
the app's, not the CLI's. Human rendering decodes only the fields it prints.

```
run inspect  { run: <run status document, incl. seeds>,
               worktreePath?, branch?, baseBranch?,
               children: [{itemId, workflowId?, goal?, state, reason?,
                           parentPhaseId?, parentUnitId?, parentAttempt?}],
               guidance?: [{text, at, ageSeconds, by, byRun?}],
               phase?:   {phaseId, attempt, status, provider?, model?, effort?,
                          cause?,
                          outputs?: {<name>: <raw JSON value>},
                          decision?, decisionTarget?, exhaustedLoops?,
                          units: [{unitId, kind, status, unitAttempt, note?,
                                   branch?, worktreePath?, threadId?}]} }
run narrative { itemId, phaseId, attempt, unitId?, unitAttempt?,
                path, present, bytes?, truncated?, content? }
```

`run status --json` carries:

- `budget`: `{kind, ceilingTokens|ceilingUsd|ceilingMillis, spentTokens,
  spentUsd, elapsedMillis, percent, estimated, unpricedRows?, exhausted,
  rootItemId?}`. Absent entirely for a run with no ceiling, and carried on `run
  inspect`'s nested `run` document the same way.
- `seeds`: the run's frozen input object.
- `pendingGuidance`: how many `run guide` entries are waiting for the run's next
  phase entry, absent at zero. `run inspect` carries the entries themselves
  under `guidance`, bounded and aged.
- `phases[]`: each entry carries `cause` (the engine's park diagnosis, absent
  when there is none) and `session` (`"continued"`, absent for a cold attempt),
  on both verbs. `outputs` / `outputOverflow` are populated by `run inspect`
  alone and absent on `run status`, whose projection stays envelope-free.
- `failedUnits[]`: each entry carries `note`, what the unit rests with (how it
  ended, or what a repair told its next try), absent when the row carries none.
  Each `phase.units` entry on `run inspect` carries it too.

`run watch --json` is NDJSON: one line per transition forwarding the app's own
objects, the run document last, because a stream cannot be one document. The
CLI's own events (gap, timeout, disconnect) are objects on the same stream
rather than prose, so every line parses.

## Commands on another computer

`agent-overflow remote list` returns enabled paired computers and their registered
projects as JSON, including an error for an unavailable computer. Use
`remote run --computer <uuid> --project <uuid> [--workspace <registered-worktree>]
[--id <request-uuid>] [--timeout <seconds>] -- <command> [args...]`. The default
timeout is one hour, maximum seven days, with four concurrent commands per host.
The generated request ID is printed before dispatch. After a missing reply,
`remote status --computer <uuid> <request-uuid>` reconciles it; retrying run must
reuse the same ID and exact arguments. `remote cancel` takes the same selectors.

Agent access is opt-in at the originating computer, and phases also need the
`remote-commands` grant. Status/cancel belongs to the initiating conversation.
Jobs outlive frontend loss. Backend shutdown interrupts them; restart never
reruns them. Output is the last 128 KiB; retention keeps 128 finished tails,
while accepted IDs remain to prevent old retries from executing again.
