# Workflow automation scheduler

The scheduler owns cron and run-event triggers, occurrence admission, skip
records, and serialized lifecycle. The application supplies run events and the
single function that starts a workflow.

## Trigger contracts

- Cron expressions use the standard five fields. Do not accept descriptors or
  seconds fields.
- Event triggers use the closed `ItemEventKind` set and match root runs in the
  automation's project.
- Reserved seed `trigger` carries structured occurrence data. `job-notes`
  carries the automation's durable continuity notes verbatim.
- Extend condition evaluation in `def`; do not create a scheduler-specific
  expression language.

## Lifecycle and admission

- One goroutine owns scheduler state, timers, event matching, and completion
  callbacks.
- Every trigger reaches the same `StartFunc`. Keep workflow resolution,
  validation, seed construction, and run creation on that path.
- Re-read an automation when it fires so edits made after arming take effect.
- Preserve a pending occurrence across a catalog re-read.
- Record every automatic refusal as a typed skip. Manual fire returns the error
  directly because a caller is waiting for an answer.
- Invalid triggers are surfaced while arming and are not represented as skips.
- On restart, compute the next cron occurrence from the current time; do not
  replay missed occurrences.
- Prevent an event automation from recursively starting itself from its own run
  events.
- Keep gate order deterministic: enabled and trigger validity, event match,
  concurrency and self-chain checks, condition, seeds, then start.
- Do not import the workflow engine. The app converts engine transitions to the
  scheduler's narrow event type.

See `docs/specs/workflows-system.md` section 11 for automations.
