# internal/workflow/wake/

The compact message a root run injects into its bound thread (spec §5, D17). Pure
text assembly over a flat, pre-resolved input; the app layer resolves the run
record and owns delivery.

Two messages, one set of rules:

- `Compose`: the run RESTED (done, failed, cancelled, needs-human).
- `ComposeProgress`: a gate took a `notify:`-decorated route and the run
  CONTINUED. Same notice, quoting, budgets, and bounded lists; the closing has to
  say the opposite (nothing is blocked, nothing is owed), because a woken agent
  assumes it was woken because something needs it.

`Signature` / `ProgressSignature` are the third export: what makes two wakes the
same ask. See Coalescing.

## Invariants

- **Pure.** No lookups, no I/O. The same input always produces the same message,
  which is what makes the format testable without a store, engine, or provider.
- **No envelope dumps.** A wake names the resting state, its typed reason, the
  phase's free text, the run's *declared* outputs, and references. Raw envelopes,
  gate traces, and diffs are reachable through the references only.
- **A bounded digest is not a dump.** Two facts ride the message because the
  alternative was measured: the run's **worktree and branch**, and, for a `gate`
  park alone, a bounded digest of what the PARKED ATTEMPT produced
  (`Input.AttemptOutputs`, the verdict a human is being asked to rule on). Both
  are resolved app-side. The digest reuses `run inspect`'s bounding rather than a
  second one, states its own overflow, and names the drill-down. Every other park
  either has no decision to make or already carries its account in the detail
  line, so it carries no digest.
- **The engine's diagnosis is its own line and a separate field.** `Run.Cause` /
  `Descendant.Cause` carry the resting attempt's persisted `park_cause`, resolved
  app-side, and render as one bounded line distinct from the detail above it. The
  detail is what the PHASE said; conflating the two would let engine prose read as
  a model's report. An absent cause renders nothing.
- **Every quoted value is data.** Goals, questions, stuck reasons, and output
  values come out of a model. They go through `internal/untrustedtext` and the
  message leads with the notice that says so, because the reader is another agent.
- **Bounded.** Outputs, references, and every free-text field carry a rune/count
  budget with an explicit "…and N more" tail. `MaxDetailRunes` (2000) is
  deliberately the largest: the question or stuck reason is the one field the
  reader has to ACT on. `MaxCauseRunes` is 400, `MaxChainRuns` 6.
- **A descendant park is composed against the root.** Child runs never bind and
  never notify as themselves; when `Input.Descendant` is set the headline is the
  root's (still `running`, because it is *waiting*) and the body names the parked
  descendant, its depth, and what it is parked on. It carries **no
  `Input.Outputs`**: those are the ROOT's declared outputs and the root has not
  finished, so on a recursive campaign they are the previous wave's carry-forward
  values restated as though they described the run that just stopped. Same rule as
  the blanked `Run.Reason`; the resolver (`app_workflow_wake.go`) omits both, and
  the descendant's own outputs already ride as `AttemptOutputs`. The body also
  carries the **call chain** root to park (`Descendant.Chain`, elided in the middle
  past `MaxChainRuns` with the elision stating how many it dropped) and a closing
  naming which run to act on, so a repair verb can be issued without a second
  command (D36a).
- **`checkpoint` is the one reason whose closing is not a fault.** The run stopped
  exactly where it was asked to (D36), so both closings point at the resume rather
  than at a resolution. Its sentence lives in `repairSentence` like every other
  reason's and both closings RETURN it rather than restating it, because
  `agent-overflow run watch` prints `RepairSentence` and nothing else: a branch
  living only in `closing` watches to a resting line naming no verb.

## The closing names the verb, not just the run (D38)

`repairSentence` appends the literal command, carrying the id of the run being
acted on (the DESCENDANT's for a descendant park), still quoted as untrusted data.

| Reason / state | Verb |
|---|---|
| `paused`, `interrupted`, `checkpoint` | `run resume` |
| a failed state | `run rerun` |
| `unit-failed` | `run retry-failed-units` / `run retry-unit`, plus `run resume` |
| `gate` park, decision is a `human:` route | `run resolve --approve\|--reject` |
| `gate` park, decision is a `park:` route | `run resume` |
| `gate` park, decision kind empty | both, keyed off `run status`'s `decision=` |
| `question` | `run answer` |
| `provider-retries-exhausted` | `run resume` |
| `provider-usage-limited` | `run resume` |
| `loop-limit-exhausted` | `run resume --phase <phase-id>` |
| legacy `retries-exhausted` | both possibilities, without guessing its source |
| `stuck` | `run resume`, with `--refresh-def` named alongside |
| everything else | no verb: the reason names its own cause |

Notes the sentences carry:

- A failed JOIN is one of the units, so the `unit-failed` closing says so. `run
  resume` continues the same attempt; `run resume --phase <id>` is the one form
  that re-runs work the wave already finished.
- A `park:` route declares no approve/reject, so naming `run resolve` for it would
  be the dead verb this sentence exists to prevent (D41 amendment). The kind rides
  in on `Run.GateDecision` / `Descendant.GateDecision`, resolved app-side from the
  gate trace.
- The `question` closing also says a phase session needs the `resolve` grant and
  that the judgment must be the reader's to make (D41).
- `provider-retries-exhausted` IS a `ContinuableReason`: the session the turn died
  in is where the next one belongs when that provider context remains available,
  otherwise the round is reconstructed in a new thread from its full persisted
  input. `provider-usage-limited` states the stronger D75 contract: an immediate
  real attempt after a reset or account switch, with no recorded availability
  state permitted to block it.
- `loop-limit-exhausted` names an earlier phase because only an outside entry
  refills its bound.
- `stuck` is NOT a `ContinuableReason`: a bare resume enters the parked phase as a
  FRESH attempt, and the usual fix is an edit to the prompt or definition it froze
  at start.

The states and reasons the closing branches on are mirrored here as package
constants rather than imported from the engine: this package is pure text assembly
over a flat input, and importing the engine for a handful of strings would drag
the whole FSM in.

## Coalescing

- **Deduplication is by CONTENT, never by a time window.** A timer answers the
  wrong question in both directions. `Signature` is over exactly the fields that
  appear in the text: run, resting state, typed reason, phase and attempt, the
  free-text detail, the engine cause, the gate kind, and for a descendant park the
  same set again for the descendant.
- **The digest, the outputs, the references, and the workspace are deliberately
  NOT in the signature.** All are derived from the coordinate that is: a run
  resting twice at the same coordinate with a fuller record has not asked a second
  question, it has had its record re-read.
- **Free text is bounded in the signature exactly as the composer bounds it.** Two
  causes differing only past `MaxCauseRunes` render byte-identical.
- **`ProgressSignature` carries the ATTEMPT, and that is load-bearing.** A
  campaign's loop-back notify fires once per wave over the same phase and route; a
  signature keyed on route alone would report wave one and swallow every wave
  after it.
- A signature is a readable string rather than a hash because it is persisted on
  the run row (`work_items.wake_signature`, v52) and read by a human debugging a
  wake that did or did not arrive. Comparison, recording, and the "somebody acted,
  so the record is spent" clear all live in `app_workflow_wake_delivery.go`.
- **A signature is recorded only once the message it identifies has stopped being
  losable, and each delivery branch records at its own durability point.** A
  session-less thread's ordinary send persists a durable row before it returns, so
  that branch records inline; a live session's message goes through the flush
  queue, which is process memory until the dispatch worker writes it or session
  recovery persists it in the composer, so that branch defers to the queue item's
  durability settlement (`triage.QueuedFlushItem`). Recording at hand-off would let
  a crash, teardown, or rollback take the message while the row swore it was
  delivered, and since the record is spent only when somebody ACTS, the identical
  wake would then be suppressed forever. Redeliver over lose.
- **A deferred record CLAIMS the row at hand-off (`queued:<signature>`) and is
  promoted by a compare-and-set.** The two writers race by construction: the record
  lands on the flush-dispatch worker, the clear on the app's serial wake queue. The
  claim is what an action spends, and the promotion is a compare-and-set against it
  (`UpdateWorkItemWakeSignatureIfCurrent`), so a spent claim can never become a
  record. **The invalidation therefore lives where the clear already lives**: route
  every clear through `clearWakeRecord`, so a future third clear site cannot miss
  it. A claim never suppresses (comparison is for equality, and a real signature
  always starts `kind=rest ` / `kind=progress `), so one stranded by a crash makes
  the run more talkative, never silent.
- **Provider usage storms add one app-side correlation layer above content
  identity (D75).** A typed usage-limit park's phase or failed units point at a
  durable provider/account/credential scope. Bound roots sharing that scope and
  watching thread claim one notification generation in
  `app_workflow_usage_attention.go`; a transition back to `running` advances it,
  and queued delivery settles by tokenized compare-and-set. This package stays
  unaware of that state; mixed failures and attribution/storage errors deliberately
  fall back to ordinary per-run wakes.

## Extension points

- A new reference kind is a new `Reference{Label, Value}` at the call site; the
  composer does not enumerate kinds.
- **A failed-unit reference says WHY that unit rests, not only which one it is.**
  The resolver (`workflowFailedUnitReferences`) renders `<unit-id> (thread <id>):
  <note>`, bounded at `maxFailedUnitNoteRunes` so the id and the thread, the two
  things a repair verb takes, can never be the part that is cut. A pause tears its
  in-flight units down `failed` carrying an interrupted note (there is no
  interrupted unit status), so a reference naming the status alone told an operator
  who paused a healthy run that their own units had failed.
- **A new field that changes the ask goes in the signature too.** If a reader would
  act differently on the new value, two wakes differing only in it are not
  duplicates; if not, it is elaboration and stays out.
- **A reference must resolve.** The composer renders whatever it is handed, so the
  rule lives with the resolver (`workflowNarrativeReference`): a narrative path is
  included only when the file is on disk. Same for the descendant's `called run
  narrative`.
- A new resting state gets a `closing` branch. The default already reads correctly
  for a terminal state, so the branch is about precision, not correctness.

## Anti-patterns

- Do NOT reach into the store or the engine from here. If the composer needs a new
  fact, resolve it in `app_workflow_wake.go` and add a field to the input.
- Do NOT bypass `untrustedtext` for a field a model can write.
- Do NOT grow a second composer for a new TRIGGER. `ComposeProgress` is not one: a
  resting wake and a progress wake are different *messages*, one naming a verb the
  reader owes and one existing to say no verb is owed, and they share every rule
  and body writer they can. A new trigger reporting a run resting, or continuing,
  belongs in the message that already exists.
- Do NOT make a progress wake reach for a surface a park uses. It is inert for an
  unbound run by design: progress is not an interruption, and the OS notification
  belongs to runs that need a human.

## References

- `docs/specs/workflows-system.md` §5, `docs/specs/workflows-system-decisions.md`
  D17 (plus the 2026-07-25 amendment that made descendant parks surface at the
  root).
- `app_workflow_wake.go` (resolution), `app_workflow_wake_delivery.go` (the one
  delivery + coalescing decision point), `app_workflow_wake_progress.go` (the
  progress resolver).
