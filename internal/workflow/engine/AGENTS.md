# Workflow engine

The engine owns workflow run state, transitions, resource admission, recovery,
fan-out, calls, budgets, and repair actions. `Runner` executes one element;
application packages resolve definitions and profiles, persist UI concerns, and
deliver wake messages.

## State ownership and recovery

- One command-loop goroutine owns mutable engine state and all FSM transitions.
  Runner callbacks re-enter through commands rather than mutating state.
- `teardown` is the sole resource-release path and the sole caller of
  `Runner.Stop`. Keep cancellation, pause, failure, and stale callbacks on that
  path.
- SQLite is the recovery journal. Rebuild running and parked work from persisted
  rows, and convert work interrupted by process loss into explicit recoverable
  state.
- There is no queued run state. Admission enters `running`; resource waiters use
  the FIFO maintained by the engine.
- Never mutate an ordinary chat session to preserve workflow execution. Workflow
  ownership lives in run and phase threads.

## Transitions and repair

- Map runner failures by typed sentinel errors, never by message text. Persist
  every engine-diagnosed park cause on the attempt.
- A bare resume preserves a continuable attempt. A phase-targeted resume starts
  a fresh entry. Keep `ContinuableReason` as the shared classifier.
- Pause and soft stop operate over the whole call tree and complete teardown
  before reporting success. Cancel is terminal and tree-aware.
- If a continuable provider context is unavailable, reconstruct the same round
  from persisted input on a new phase thread.
- A human gate requires an explicit resolution. A targeted resume may leave that
  phase according to the normal route rules.
- Provider usage limits are typed parks. Resume must make a real attempt and
  cannot be blocked by stale availability observations.
- Loop bounds are derived from persisted attempts for a fresh phase entry. A
  targeted re-entry resets the applicable bound.

## Runtime context

- Pass `attemptRef` explicitly; never infer the current attempt from mutable
  item state while constructing inputs.
- Compose `history.<phase>` and `budget` here from persisted state, using the
  same budget resolver used by admission checks.
- Enforce budgets before every attempt against the root run. Item overrides and
  profile defaults must pass through `ResolveBudget`.
- Acquire resource names in sorted order and release them only through teardown.
  A fan-out phase itself consumes no provider slot; its units and join do.
- Keep access and grants consistent across fresh starts, continuations, unit
  turns, and reconstructed sessions.

## Guidance, feedback, and events

- Guidance and feedback persistence is ordered for recoverability rather than
  made falsely atomic. Acknowledgement occurs only after the send path accepts
  the rendered block.
- Only elements that actually deliver an agent prompt consume a guidance slot.
  Heal undecodable slots visibly and continue with the durable remaining data.
- Redelivery includes every prior attempt of the phase still awaiting
  acknowledgement.
- Construct `workflow:phase-state` through `emitPhaseState` so persisted state,
  event payloads, and watch transitions stay aligned.
- A gate `notify` event announces progress after the route transition. It is not
  a park and must not create a repair obligation.

## Fan-out and calls

- Expand fan-out once per phase attempt and enforce the project width ceiling at
  that boundary. Recovery uses persisted units.
- The join runs after all units rest and is the final unit of the attempt. It is
  retryable and cannot be dropped.
- Route unit repair through `reopenUnit` so try numbers, persisted status, and
  resources advance together. Batch retry is one command-loop operation.
- Call phases and call units share planning, child creation, completion, depth,
  cancellation, and output synthesis. Child completion must notify the parent by
  command, never by nested transition execution.
- Leaving a call edge for any reason tears down its active descendants.

Keep provider sessions, watchdogs, subprocesses, worktrees, definition lookup,
profile loading, scheduler policy, wire methods, and wake delivery behind the
engine interfaces. See `docs/specs/workflows-system.md`.
