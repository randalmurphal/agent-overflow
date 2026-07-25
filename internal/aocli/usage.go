package aocli

// Every usage string the binary prints. They live together so the command tree
// is readable in one place, and so adding a subcommand without documenting it is
// an obvious omission rather than a hidden one.

const rootUsage = `Usage: ao [--config-root <path>] <command> [options]

Offline commands (work anywhere):
  workflow new       Scaffold a workflow definition
  workflow validate  Validate a workflow definition
  workflow list      List resolved workflow definitions
  workflow schema    Print the workflow authoring JSON schema

Session commands (run inside an Agent Overflow agent session):
  run                Start, observe, and control workflow runs
  notes              Read and write an automation's continuity notes
  schedule           Create a cron automation for a workflow

Exit codes: 0 success, 1 a run finished in a state other than done, 2 error.
`

const workflowUsage = `Usage: ao workflow <command> [options]

Commands:
  new       Scaffold a workflow definition
  validate  Validate a workflow definition
  list      List resolved workflow definitions
  schema    Print the workflow authoring JSON schema
`

const validateUsage = `Usage: ao workflow validate [options] <path>
       ao workflow validate [options] --id <id>

Options:
  --config-root <path>  override the Agent Overflow config root
  --id <id>             resolve and validate a workflow by id
  --json                write the typed validation result as JSON
  --project <slug>      include workflows for the project slug
`

const listUsage = `Usage: ao workflow list [options]

Options:
  --config-root <path>  override the Agent Overflow config root
  --json                write the resolved workflow list as JSON
  --project <slug>      include workflows for the project slug
`

const schemaUsage = `Usage: ao workflow schema

Prints the JSON schema every workflow definition is authored against. Offline:
it is compiled into this binary and needs no running app.
`

const runUsage = `Usage: ao run <command> [options]

Commands:
  start <workflow-id>     Start a run and print its id
  status <run-id>         Print one run's current state
  wait <run-id>           Block until a run stops doing work
  output <run-id>         Print a run's declared outputs and artifacts
  list                    List this project's runs
  pause <run-id>          Park a run and everything below it
  resume <run-id>         Return a parked run to running
  cancel <run-id>         Stop a run for good
  rerun <run-id>          Start a failed run's last phase again
  retry-unit <run-id> <unit-id>
                          Re-run one failed unit of a parked fan-out attempt
`

const runStartUsage = `Usage: ao run start [options] <workflow-id>

Options:
  --base-branch <name>  branch the run's worktree starts from
  --goal <text>         one-line goal recorded on the run
  --json                write the app's result as JSON
  --scope <scope>       resolve the workflow in this scope (shared|project)
  --seed <key=value>    seed one declared input (repeatable; JSON values parsed)
  --step                pause at every gate decision
  --timeout <duration>  with --wait, give up waiting after this long
  --wait                block until the run stops doing work
`

const runStatusUsage = `Usage: ao run status [--json] <run-id>
`

const runWaitUsage = `Usage: ao run wait [--json] [--timeout <duration>] <run-id>

Exits 0 when the run finished done, 1 when it rested in any other state.
`

const runOutputUsage = `Usage: ao run output [--json] <run-id>
`

const runListUsage = `Usage: ao run list [--active] [--json]
`

const runPauseUsage = `Usage: ao run pause [--json] <run-id>
`

const runResumeUsage = `Usage: ao run resume [--json] [--phase <phase-id>] <run-id>
`

const runCancelUsage = `Usage: ao run cancel [--json] <run-id>
`

const runRerunUsage = `Usage: ao run rerun [--json] [--guidance <text>] <run-id>
`

const runRetryUnitUsage = `Usage: ao run retry-unit [--json] [--note <text>] <run-id> <unit-id>
`

const notesUsage = `Usage: ao notes <command> [options]

Commands:
  get <automation-id>  Print an automation's continuity notes
  set <automation-id>  Replace them with stdin, or with --file
`

const notesGetUsage = `Usage: ao notes get [--json] <automation-id>
`

const notesSetUsage = `Usage: ao notes set [--json] [--file <path>] <automation-id>

Reads the new notes from stdin unless --file names a file. Empty input clears
the notes.
`

const scheduleUsage = `Usage: ao schedule [options] --cron <expr> <workflow-id>

Options:
  --cron <expr>       five-field cron expression naming when to start (required)
  --json              write the app's result as JSON
  --name <text>       name shown in the automations list
  --scope <scope>     resolve the workflow in this scope (shared|project)
  --seed <key=value>  seed one declared input (repeatable; JSON values parsed)
`
