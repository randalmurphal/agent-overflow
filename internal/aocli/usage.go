package aocli

// Every usage string the binary prints. They live together so the command tree
// is readable in one place, and so adding a subcommand without documenting it is
// an obvious omission rather than a hidden one.

const rootUsage = `Usage: agent-overflow [--config-root <path>] <command> [options]

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

const workflowUsage = `Usage: agent-overflow workflow <command> [options]

Commands:
  new       Scaffold a workflow definition
  validate  Validate a workflow definition
  list      List resolved workflow definitions
  schema    Print the workflow authoring JSON schema
`

const validateUsage = `Usage: agent-overflow workflow validate [options] <path>
       agent-overflow workflow validate [options] --id <id>

Options:
  --config-root <path>  override the Agent Overflow config root
  --id <id>             resolve and validate a workflow by id
  --json                write the typed validation result as JSON
  --project <slug>      include workflows for the project slug
                        (defaults to AO_PROJECT inside an app session)
`

const listUsage = `Usage: agent-overflow workflow list [options]

Options:
  --config-root <path>  override the Agent Overflow config root
  --json                write the resolved workflow list as JSON
  --project <slug>      include workflows for the project slug
                        (defaults to AO_PROJECT inside an app session)
`

const schemaUsage = `Usage: agent-overflow workflow schema

Prints the JSON schema every workflow definition is authored against. Offline:
it is compiled into this binary and needs no running app.
`

const runUsage = `Usage: agent-overflow run <command> [options]

Commands:
  start <workflow-id>     Start a run and print its id
  status <run-id>         Print one run's state, its caller, and any failed units
  wait <run-id>           Block until a run stops doing work
  output <run-id>         Print a run's declared outputs and artifacts
  list                    List this project's runs and how they call each other
  pause <run-id>          Park a run and everything below it
  resume <run-id>         Continue a parked run; --phase <id> starts that phase over
                          (--refresh-def re-reads the definition from disk)
  cancel <run-id>         Stop a run for good
  rerun <run-id>          Start a failed run's last phase again
  retry-unit <run-id> <unit-id>
                          Re-run one failed unit of a parked fan-out attempt
                          (the join is one of them)
  retry-failed-units <run-id>
                          Re-run every failed unit of a parked fan-out attempt
  soft-stop <run-id>      Stop a run tree at its next call boundary
  resolve <run-id>        Approve or reject a run parked on a gate
  answer <run-id> <text>  Answer a run parked on a question
`

const runStartUsage = `Usage: agent-overflow run start [options] <workflow-id>

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

const runStatusUsage = `Usage: agent-overflow run status [--json] <run-id>
`

const runWaitUsage = `Usage: agent-overflow run wait [--json] [--timeout <duration>] <run-id>

Exits 0 when the run finished done, 1 when it rested in any other state.
`

const runOutputUsage = `Usage: agent-overflow run output [--json] <run-id>
`

const runListUsage = `Usage: agent-overflow run list [--active] [--json]
`

const runPauseUsage = `Usage: agent-overflow run pause [--json] <run-id>
`

const runResumeUsage = `Usage: agent-overflow run resume [--json] [--phase <phase-id>] [--refresh-def] <run-id>

Continues the parked run and preserves what it already finished: a stopped turn
carries on in its own session, and a fan-out reopens only what is blocking it
while every finished unit keeps its result — including the runs its units called,
which are never re-executed.

--phase <phase-id> is the start-over. It enters that phase fresh, from outside
every loop through it, so loop budgets refill; a fan-out there expands a new wave
and calls its child runs again. Naming the parked phase itself is a legitimate
way to ask for exactly that.

--refresh-def is the repair for "I edited the prompt of a parked phase". The
definition a run froze at start is what it runs, every attempt, so an edit made
while the run was parked is invisible to it; --refresh-def re-reads the workflow
and its prompt files from disk for this entry and runs the edited version from
here on. It applies at a fresh phase entry only — a bare resume on a run parked
paused, interrupted, checkpoint, or unit-failed continues an attempt whose work
was launched under the frozen definition, and is refused unless --phase says to
discard it. Between campaign waves nothing is needed: a call reads its target
from disk every time it is made.

A gate decision is not resume's to take: run resolve settles a human: route, and
run retry-unit / run retry-failed-units repair one unit or all of them — a failed
join included, since it is a unit of the attempt like any other.
`

const runCancelUsage = `Usage: agent-overflow run cancel [--json] <run-id>
`

const runRerunUsage = `Usage: agent-overflow run rerun [--json] [--guidance <text>] [--refresh-def] <run-id>

Starts a failed run's last phase again, carrying its diagnosis — and --guidance,
when given — into the new attempt.

--refresh-def re-reads the workflow and its prompt files from disk for that
attempt. Without it the run renders the definition it froze at start, which is
what every attempt of a run does; with it, a definition edited since the failure
is what runs from here on.
`

const runRetryUnitUsage = `Usage: agent-overflow run retry-unit [--json] [--note <text>] <run-id> <unit-id>
`

const runRetryFailedUnitsUsage = `Usage: agent-overflow run retry-failed-units [--json] [--note <text>] <run-id>

Re-runs every unit of the parked fan-out attempt that is resting failed, in one
action — the join among them, since a join that failed is a failed unit of the
attempt. Finished units keep their results, units under human steering are left
alone, and the repaired ones queue for provider capacity like any other work.
`

const runSoftStopUsage = `Usage: agent-overflow run soft-stop [--json] [--clear] <run-id>

Asks a run tree to stop at its NEXT call boundary: the run keeps going, nothing
in flight is interrupted, and the next time it would invoke another run it parks
needs-human(checkpoint) instead. Resuming takes the call it skipped, so a
campaign continues exactly where it stopped.

The request is set on the ROOT of a tree and every run below it honours it; a
called run is refused with the run to set it on instead. Setting it twice is one
request, and --clear withdraws it. The boundary that fires consumes the request,
so a resume does not stop again.

A run whose workflow makes no calls has no boundary to stop at. The request is
accepted and simply never fires — nothing else is interrupted in its place.
`

const runResolveUsage = `Usage: agent-overflow run resolve [--json] [--note <text>] --approve|--reject <run-id>

Settles a run resting needs-human(gate) by taking one of the two routes the
workflow's gate declared. Exactly one of --approve and --reject is required:
they are the decision, not a default with an override. --note is recorded with
the decision and, on a reject, carried into the loop the gate sends the run
back through.

It applies only to a human: route — the form that declares an approve target
and a reject loop. A park: route rests under the same gate reason but declares
no continuation at all, so there is nothing here to select; "run resume"
re-enters its phase once the cause is addressed. "run status" shows which kind
a parked attempt is under its decision= field.

A phase session needs the "resolve" grant to use this; an interactive session
has it already, because a human approves each invocation.
`

const runAnswerUsage = `Usage: agent-overflow run answer [--json] <run-id> <text>

Answers a run resting needs-human(question) and returns it to running. The
answer reaches the phase that asked, as the human's would.

A phase session needs the "resolve" grant to use this; an interactive session
has it already, because a human approves each invocation.
`

const notesUsage = `Usage: agent-overflow notes <command> [options]

Commands:
  get <automation-id>  Print an automation's continuity notes
  set <automation-id>  Replace them with stdin, or with --file
`

const notesGetUsage = `Usage: agent-overflow notes get [--json] <automation-id>
`

const notesSetUsage = `Usage: agent-overflow notes set [--json] [--file <path>] <automation-id>

Reads the new notes from stdin unless --file names a file. Empty input clears
the notes.
`

const scheduleUsage = `Usage: agent-overflow schedule [options] --cron <expr> <workflow-id>

Options:
  --cron <expr>       five-field cron expression naming when to start (required)
  --json              write the app's result as JSON
  --name <text>       name shown in the automations list
  --scope <scope>     resolve the workflow in this scope (shared|project)
  --seed <key=value>  seed one declared input (repeatable; JSON values parsed)
`
