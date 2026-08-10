package aocli

// Every usage string the binary prints. They live together so the command tree
// is readable in one place, and so adding a subcommand without documenting it is
// an obvious omission rather than a hidden one.

const rootUsage = `Usage: agent-overflow <command> [options]

Offline commands (work anywhere):
  workflow new       Scaffold a workflow definition
  workflow validate  Validate a workflow definition
  workflow list      List resolved workflow definitions
  workflow schema    Print the workflow authoring JSON schema

Session commands (run inside an Agent Overflow agent session):
  run                Start, observe, and control workflow runs
  memory             Record and read this campaign's accumulated lessons
  notes              Read and write an automation's continuity notes
  schedule           Create a cron automation for a workflow

Exit codes: 0 success, 1 the asked-about thing said no (a run resting in a
state other than done, validation findings, an absent record), 2 error.
"run watch" adds two of its own: 3 its --timeout expired, 4 the app stopped
answering.

Put --config-root after the subcommand ("workflow validate --config-root …"):
the app binary routes to these commands by their leading verb, so an invocation
that starts with a flag never reaches them.
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
  inspect <run-id>        Print one run whole: worktree, branch, seeds, children,
                          attempts, and each phase's latest outputs
                          (--phase <id> reads that attempt whole)
  narrative <run-id> --phase <id>
                          Print one attempt's narrative (--unit <id> for a unit)
  wait <run-id>           Block until a run stops doing work
  watch <run-id>          Block and print every state change as it happens
                          (--tree includes the runs it called)
  output <run-id>         Print a run's declared outputs and artifacts
  amend <run-id> --seed k=v
                          Change a resting run's seeds without restarting it
  guide <run-id> <text>   Leave an instruction for a run's next phase entry
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

const runInspectUsage = `Usage: agent-overflow run inspect [--json] [--phase <phase-id> [--attempt <n>]] <run-id>

Prints one run whole, in one call: everything "run status" shows, plus the
worktree and branch its work happens on, the base branch it was cut from, the
seeds it froze at start, the runs it called and what called them, and — for the
latest attempt of each phase — a bounded digest of that attempt's envelope
outputs. A digest that left values out says how many. Any "run guide" entries
still waiting for the run's next phase entry print here too, oldest first, with
who left each one and how long it has been waiting; "run status" prints only
their count.

--phase <phase-id> reads one attempt whole instead: its full envelope outputs,
its gate decision, and the fan-out units it expanded with each unit's status,
try, branch, and worktree. The digests are dropped when it is given — naming an
attempt is how a caller says the digest was not enough, and printing both would
print the same values twice. --attempt <n> reads an older attempt of that phase
instead of its latest, and means nothing without --phase.

Envelope output values are quoted: they were written by a model, and this output
is usually read by one.
`

const runNarrativeUsage = `Usage: agent-overflow run narrative [--json] [--attempt <n>] [--unit <unit-id>] --phase <phase-id> <run-id>

Prints the narrative one phase attempt wrote — the human-readable account of
what it did, decided, and validated. --phase is required; the attempt defaults
to that phase's latest, which is the one a parked run is resting on.

--unit <unit-id> reads one fan-out unit's account instead of the phase's, on
that unit's current try. "run inspect --phase <id>" lists the unit ids.

An attempt that wrote no narrative exits 1 and names the path that was looked
for: it ran, and left no account. A run, phase, attempt, or unit that does not
exist is an error and exits 2.
`

const runWaitUsage = `Usage: agent-overflow run wait [--json] [--timeout <duration>] <run-id>

Exits 0 when the run finished done, 1 when it rested in any other state.
`

const runWatchUsage = `Usage: agent-overflow run watch [--json] [--tree] [--timeout <duration>] <run-id>

Blocks and prints one line per state change as it happens, ending when the run
comes to rest. Nothing here polls: the app holds each request until the run
moves, so a watch that sits quiet for six hours has made a handful of calls and
read nothing in between. Use it instead of a sleep loop.

--tree also reports the runs this one called, transitively, including waves that
start while the watch is already running — which is what makes it the verb for
supervising a campaign rather than a phase.

--timeout gives up after a duration and exits 3 with the run still going;
without it the watch blocks indefinitely. If the app stops answering, the watch
says so and exits 4 rather than hanging: a monitor that dies has to say it died.
Transitions that were not retained (the app restarted, or they aged out) print a
gap line, and the state printed after it is read fresh — a watch never
reconstructs history it did not see.

Exit codes: 0 the run finished done, 1 it rested in any other state, 2 usage or
a refusal, 3 --timeout expired, 4 the app became unreachable.

--json writes NDJSON: one object per transition, then the app's run document as
the final line.
`

const runOutputUsage = `Usage: agent-overflow run output [--json] <run-id>
`

const runAmendUsage = `Usage: agent-overflow run amend [--json] --seed <key=value> <run-id>

Changes seed values on a run that is RESTING, without cancelling and restarting
it. Only the named seeds change; everything else the run froze is left alone,
and only inputs the run's frozen workflow declares may be named — an undeclared
key is refused naming the ones that exist. Values are typechecked against the
same input schema a "run start --seed" is.

A running run is refused: an attempt reads its seeds when it starts, so changing
them under one would leave a single attempt rendering two sets of inputs. Pause
it first, or wait. A finished run is refused too — nothing is left to read them.

The amendment is durable the moment it succeeds, and the output says WHEN the
run will read it. Usually that is the next attempt the run starts, whichever
verb starts it. Where the parked attempt is repaired in place instead — a
fan-out reopening its failed units, a call phase re-linking the child it waits
on — that attempt keeps the variables it froze, and the output names the
"run resume --phase <id>" that enters the phase fresh.

Amending a run that was CALLED by another changes its own remaining phases only.
The next run its caller starts re-evaluates the caller's arguments, so amend the
root to change what later waves are given; the output names it.
`

const runGuideUsage = `Usage: agent-overflow run guide [--json] <run-id> <text>

Leaves one instruction for a run WITHOUT parking it. The run keeps working; the
text is delivered at its next FRESH phase entry, rendered into that attempt's
prompt as a clearly labelled block of operator guidance, and the slot is cleared.

It is the opposite direction of a "notify:" gate: that tells the watching thread
what the run decided, this tells the run what the watcher wants next. Before it
existed, redirecting a free-running campaign meant pausing it, editing, and
resuming — which throws away the turn in flight and, in a fan-out, the wave under
it.

The turn in flight is NEVER interrupted. There is no mid-turn injection: a run
that is mid-phase keeps the guidance pending until it advances or loops into the
next phase. A run parked on a continuable reason (paused, interrupted,
checkpoint, unit-failed, retries-exhausted) is CONTINUED by a bare "run resume",
and a continuation is not a phase entry — the output says so and names
"run resume <id> --phase <id>" as what enters one now.

Entries accumulate in order and are all delivered together at the next entry.
Both bounds — how many may wait and how long one may be — are the app's, and its
refusal states the number rather than this page duplicating it: a call past the
limit is refused rather than burying the earlier entries. A steer belongs in a
sentence; a specification belongs in the phase's prompt file, which
"run resume --refresh-def" re-reads.

The author is stamped by the app from the calling credential, never from the
text: an interactive session is recorded as a human operator and a workflow
phase session as that phase's run, and the delivered block says which. Guiding a
run that was CALLED by another reaches that run's own remaining phases only.

A terminal run is refused, as is a done run awaiting disposition — neither has a
phase entry left. Pending entries and their ages are printed by
"agent-overflow run inspect <run-id>"; "run status" prints the count.

Text that BEGINS with a dash reads as a flag. Put a literal -- before it:
  agent-overflow run guide r-1 -- "--refresh-def is the flag you want next time"
`

const runListUsage = `Usage: agent-overflow run list [--active] [--json]
`

const runPauseUsage = `Usage: agent-overflow run pause [--json] <run-id>
`

const runResumeUsage = `Usage: agent-overflow run resume [--json] [--phase <phase-id>] [--refresh-def] [--at <when>] <run-id>

Continues the parked run and preserves what it already finished: a stopped turn
carries on in its own session, a turn that DIED on a provider failure the
transient retries could not outlast takes its next turn on that same session,
and a fan-out reopens only what is blocking it while every finished unit keeps
its result — including the runs its units called, which are never re-executed.

--phase <phase-id> is the start-over. It enters that phase fresh, from outside
every loop through it, so loop budgets refill; a fan-out there expands a new wave
and calls its child runs again. Naming the parked phase itself is a legitimate
way to ask for exactly that. Naming an EARLIER one is what a retries-exhausted
park wants when what ran out was a loop bound rather than the provider: the
bound refills when the loop's target is entered from outside the cycle, which a
bare resume — a continuation — never does.

--refresh-def is the repair for "I edited the prompt of a parked phase". The
definition a run froze at start is what it runs, every attempt, so an edit made
while the run was parked is invisible to it; --refresh-def re-reads the workflow
and its prompt files from disk for this entry and runs the edited version from
here on. It applies at a fresh phase entry only — a bare resume on a run parked
paused, interrupted, checkpoint, unit-failed, or retries-exhausted continues an
attempt whose work was launched under the frozen definition, and is refused
unless --phase says to discard it. Between campaign waves nothing is needed: a
call reads its target from disk every time it is made.

--at <when> schedules the bare resume above instead of taking it now, on any
park a bare resume continues. <when> is RFC 3339 (2026-08-15T19:56:00Z) or a
duration from now (+36h), resolved against the app's clock, and the command
prints the moment it armed. The run stays exactly where it is until then, and
anything that repairs it in the meantime — a resume, a cancel, a discard —
disarms the schedule. It is the manual half of what a dated usage-limit refusal
already does on its own: a provider that refuses a turn and says when the
allowance returns parks the run retries-exhausted and comes back by itself, so
--at is for the case where you know the time and the provider did not say it.

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

An answer that BEGINS with a dash reads as a flag. Put a literal -- before it:
  agent-overflow run answer r-1 -- "-Werror stays; fix the warnings instead"
`

const memoryUsage = `Usage: agent-overflow memory <command> [options]

Commands:
  add   Record one durable lesson for later work in this campaign
  list  Print the campaign's recorded notes

A campaign's memory is the whole run TREE's, keyed by its root run, and it
outlives every run in it. Every element's prompt already carries a bounded,
newest-first digest of it; this is how notes get in, and how to read past the
digest's budget.
`

const memoryAddUsage = `Usage: agent-overflow memory add [options] --kind <kind> "<note>"

Options:
  --kind <kind>   pattern | warning | learning | handoff (required)
  --file <path>   cite one path as evidence (repeatable)
  --json          write the app's result as JSON
  --run <run-id>  record against this run instead of the calling phase's own

Kinds:
  pattern   a shape that worked and should be repeated
  warning   a trap: something that looked right and was not
  learning  a fact about the environment, codebase, or tools
  handoff   state you are deliberately leaving for the next element

Write for a reader with NO context: they see your text and nothing else. Do not
restate the diff, report status, or narrate what you did — that is the
narrative's job. Provenance and timestamps are stamped by the system.

A note that BEGINS with a dash reads as a flag. Put a literal -- before it:
  agent-overflow memory add --kind learning -- "- rebase before merging a lane"
`

const memoryListUsage = `Usage: agent-overflow memory list [--kind <kind>] [--run <run-id>] [--json]

Prints the campaign's notes oldest first, with the element and wave that wrote
each. Unreadable lines (a note torn by a crash) are counted in the header
rather than hidden.
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
