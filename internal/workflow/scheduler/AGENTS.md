# internal/workflow/scheduler/

Automations (spec §11): cron and internal-event triggers that start
workflow runs through the app's one start path. One goroutine, one
timer, no executor of its own.

## Trigger shapes

Stored on `automations.trigger` as JSON. Unknown fields are refused, so
a typo surfaces on write instead of disabling a schedule silently.

```json
{"kind":"cron","expr":"0 3 * * *"}
{"kind":"event","on":"item-done"}
{"kind":"event","on":"item-failed","workflowId":"nightly-audit"}
```

- `cron` takes the standard **five** fields only. `@daily` and
  `@every 30s` are rejected before the parser sees them: sub-minute
  granularity would spin the one timer this package owns, and one
  grammar is easier to explain on an automation row than three.
- `event.on` is the closed set `item-done` / `item-failed` /
  `item-needs-human` (a run coming to rest), which is the only kind of
  transition another run can meaningfully react to. `cancelled` is
  deliberately not a member: a human stopping a run is not a result.
- An event trigger matches **root runs in the automation's own project**.
  A called run's transitions are its tree's internals; matching them
  would let one run tree fan a chain storm. An empty `workflowId`
  matches every workflow in the project.

`condition` (the run-if) is a `def.Predicate`, the same expression
objects a phase gate routes on:

```json
{"all":[{"eq":{"ref":"trigger.event.state","value":"done"}},
        {"exists":"job-notes"}]}
```

It is evaluated through `def.EvaluatePredicate` against exactly the
variable map the run would be seeded with. There is no second
evaluator: `def` owns predicate shape (`ValidatePredicateShape`) and
predicate semantics, this package only decodes and calls.

## Reserved seeds

Every fired run carries two seeds the scheduler binds **last**, so a
stored seed cannot shadow them (the rule `def` applies to a join's
`units`):

- `trigger` is `{kind, fired-at, scheduled-for?, event?{kind, item-id,
  workflow-id, state, reason}}`.
- `job-notes` is the automation's continuity notes, verbatim. The point
  of a scheduled job's notes is that the next run reads them.

Both names are kebab-case because that is the only identifier grammar
`def` can reference; `job_notes` could be neither a declared workflow
input nor a `{{...}}` reference, so it would be an inert seed. Creating
or updating an automation with a stored seed under either name is
refused (`app_workflow_bindings.go`).

## Invariants

- **One goroutine owns everything.** Cron fires, event matches, and Run
  now are all commands on the same loop, so an automation's overlap
  check can never race another fire of the same automation. Commands
  are synchronous. The caller gets the fire's real error.
- **Never imports the engine.** Internal events arrive because the app
  feeds them in (`NotifyItemEvent`) from the same state-event listener
  that wakes bound threads, through an ordered `serialQueue` so the
  engine's goroutine is never blocked on a scheduler decision.
- **One start path.** `StartFunc` is the app's `startWorkflowRun` with
  source `automation` and the automation's id as source ref. The
  scheduler starts nothing itself, and the overlap probe reads back
  exactly that pair.
- **A fire reads the automation when it fires.** The loop's snapshot
  decides only *whether* something is due; `attempt` takes an id and
  re-reads the row. Job notes, seeds, and the run-if condition can all
  be edited between arming an occurrence and its coming due, and
  continuity notes exist precisely so the next run reads what the last
  one left. It also makes the three fire paths identical instead of
  Run now being the only one that saw current data.
- **Skips are recorded, never implied.** Every refusal of a scheduled
  fire writes `skip_count` / `last_skip_at` / `last_skip_reason` on the
  automation row. A fire that started a run writes `last_fired_at` /
  `last_run_item_id`. "Nothing happened" is never a state you have to
  infer from silence.
- **A manual fire errors instead of recording a skip.** Run now has a
  human present to read the reason; a recorded skip would also be
  indistinguishable from a schedule that misbehaved.
- **A broken trigger is surfaced, not skipped.** `arm` reports an
  unparseable stored trigger once per stored version (`Report`), and
  `WorkflowListAutomations` carries the standing `triggerError` on the
  row a human acts on. A trigger is parsed on write *and* on every load
  precisely so a hand-edited row cannot become a schedule that quietly
  never fires.
- **No replay on restart.** Next fires are computed from `now` at
  `Start`. An occurrence whose time passed while the app was closed is
  not fired and is not recorded as a skip: nothing was skipped, the app
  was off. Recording one would make an overnight shutdown look like a
  malfunctioning automation, and firing one would stampede every
  automation at boot.
- **A pending occurrence survives a re-read.** `arm` re-reads the
  enabled set every iteration (the table is the truth), but a computed
  occurrence is held in `armedFire` keyed on the stored trigger text.
  `Schedule.Next` only ever answers strictly *after* the time it is
  given, so recomputing a due occurrence would roll it silently
  forward, which is what a command landing in the same instant as a
  fire used to do.
- **Self-chaining is an authoring accident.** An automation whose own
  run's completion re-matches its trigger records a `self-chain` skip.
  Cycles across two automations stay legal. Those are deliberate.

## Gate order (one fire)

`fire.go#attempt` is the only path from "a trigger fired" to "a run
started, or a skip was recorded". Cheapest and most specific first, so
the recorded reason is the one that actually blocked the fire:

| Gate | Scheduled fire | Run now |
|---|---|---|
| row deleted since arming | dropped (no row to record on) | error |
| row disabled since arming | dropped (no schedule to skip) | starts, by design |
| row unreadable | error to `Report` | error |
| self-chain | skip `self-chain: run <id> …` | n/a (no event) |
| overlap probe failed | skip `overlap check failed: …` | error |
| a run is `running`/`needs-human` | skip `run <id> is still …` | error |
| seeds unusable | skip `seeds are unusable: …` | error |
| condition errored | skip `condition error: …` | **bypassed** |
| condition false | skip `condition false` | **bypassed** |
| start failed | skip `start failed: …` | error |
| started | record fire | record fire |

Not knowing whether a run is active is not permission to start one.
A failed probe refuses like a found run does.

## robfig/cron boundary

`cron.ParseStandard` and `Schedule.Next` only. The library's `cron.Cron`
runner, its goroutine, its logger, and its job-wrapper chain are not
used and must not be introduced: the app owns the timer so that fires,
CRUD refreshes, and internal events are serialized against each other
and testable against a fake clock.

## Testing

`harness_test.go` supplies a fake clock and a fake store; nothing here
needs SQLite, an engine, or a provider. Loop behaviour is asserted
under `-race -count=10` because the interesting failures are races
between a due timer and a queued command.

## Anti-patterns

- Do NOT import `internal/workflow/engine`. If the scheduler needs a
  new fact about a run, the app feeds it in on `ItemEvent`.
- Do NOT add a second evaluator for conditions. Extend `def`.
- Do NOT let a refusal return `nil` with nothing written. Every branch
  in `attempt` past the load either starts a run, records a skip, or
  returns an error. The two silent drops are the load gates above,
  where the automation itself stopped existing or stopped being armed.
  They are the only two.
- Do NOT widen `ItemEventKind` without asking whether the new member is
  a run *coming to rest*. The set is closed on purpose.

## References

- `docs/specs/workflows-system.md` §11 — automations.
- `app_workflow_bindings.go` — the RPC surface and its validation.
- `internal/workflowapp/runtime.go` — lifecycle and the event queue;
  `app_workflow.go` retains the start callback adapter.
- `internal/store/automations.go` — definitions, notes, fire records,
  and the active-run probe.
