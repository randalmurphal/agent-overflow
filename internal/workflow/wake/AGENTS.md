# internal/workflow/wake/

The compact message a root run injects into its bound thread (spec §5,
decision D17). Pure text assembly over a flat, pre-resolved input; the
app layer resolves the run record and owns delivery.

There are **two** messages, and one set of rules:

- `Compose` — the run RESTED (done, failed, cancelled, needs-human).
- `ComposeProgress` — a gate took a `notify:`-decorated route and the run
  CONTINUED (K1). Same notice, same quoting, same budgets, same bounded
  lists; the difference is the closing, which has to say the opposite —
  nothing is blocked, nothing is owed — because a woken agent's default
  assumption is that it was woken because something needs it.

`Signature` / `ProgressSignature` are the third export: what makes two
wakes the same ask (K2). See "Coalescing" below.

## Files

| File | Responsibility |
|---|---|
| `input.go` | Budgets, the data notice, and the flat input records. |
| `compose.go` | The resting message and every body writer both messages share. |
| `closing.go` | The resting closing: the reason→verb repair map (D38) and the mirrored state/reason constants it branches on. |
| `progress.go` | The progress message. |
| `signature.go` | The coalescing identity. |

## Invariants

- **Pure.** `Compose` / `ComposeProgress` perform no lookups and no I/O.
  The same input always produces the same message, which is what makes
  the format testable without a store, an engine, or a provider.
- **No envelope dumps.** A wake names the resting state, its typed
  reason, the phase's free text, the run's *declared* outputs, and
  references. Raw envelopes, gate traces, and diffs are reachable
  through the references — they never ride the message.
- **A bounded digest is not a dump (K3).** Two facts ride the message
  because the alternative was measured, not because they fit: the run's
  **worktree and branch** (asked twelve times in one live campaign), and
  — for a `gate` park alone — a bounded digest of what the PARKED
  ATTEMPT produced (`Input.AttemptOutputs`, the verdict/severity a human
  is being asked to rule on, read before *every* gate resolution in that
  same campaign). Both are resolved app-side. The digest reuses `run
  inspect`'s bounding rather than a second one, states its own overflow,
  and names the drill-down that answers it. Every other park either has
  no decision to make or already carries its account in the detail line,
  so it carries no digest — the message is compact because the lists are
  chosen, not because everything is short.
- **The engine's diagnosis is its own line, and a separate field.**
  `Run.Cause` / `Descendant.Cause` carry the resting attempt's persisted
  `park_cause` — resolved app-side from the attempt row, never looked up
  here — and render as one bounded line ("The engine stopped it here:")
  distinct from the detail above it. The detail is what the PHASE said;
  conflating the two would let engine prose read as a model's report. An
  absent cause renders nothing: an empty label would read as a diagnosis
  that was lost on the way here.
- **Every quoted value is data.** Goals, questions, stuck reasons, and
  output values come out of a model. They go through
  `internal/untrustedtext` and the message leads with the notice that
  says so, because the reader is another agent.
- **Bounded.** Outputs, references, and every free-text field carry a
  rune/count budget with an explicit "…and N more" tail. A run that
  produced a thousand outputs still composes a message a thread can
  take. `MaxDetailRunes` is deliberately the largest of them: the
  question or stuck reason is the one field the reader has to ACT on,
  and a wake that halves it buys its compactness by making the round
  trip it exists to prevent mandatory.
- **A descendant park is composed against the root.** Child runs never
  bind and never notify as themselves; when `Input.Descendant` is set
  the headline is the root's (still `running` — it is *waiting*) and the
  body names the parked descendant, its depth, and what it is parked on.
  This is what turns "a grandchild is stuck" into one message on the
  surface a human or agent actually watches. The body also carries the
  **call chain** root→park (`Descendant.Chain`, elided in the middle
  past `MaxChainRuns` with the elision stating how many it dropped) and
  a closing naming which run to act on, because a campaign's sixth wave
  is a run the reader has never seen and the message has to be enough to
  issue a repair verb against it without a second command (D36a).
- **`checkpoint` is the one reason whose closing is not a fault.** The
  run stopped exactly where it was asked to (D36), so both the root and
  the descendant closing say that and point at the resume rather than at
  a resolution. Its sentence lives in `repairSentence` like every other
  reason's, and both closings return it rather than restating it: the
  descendant branch adds only the lead-in naming which run stopped, and
  the root branch is the sentence alone (with no "parked and does not
  continue" preamble, which would report the stop as something owing
  resolution). That matters beyond tidiness — `agent-overflow run watch`
  prints `RepairSentence` and nothing else, so a checkpoint branch that
  existed only in `closing` watched to a resting line naming no verb, on
  the one park a supervising agent produces for itself.
- **A closing names the verb, not just the run (D38).** `repairSentence`
  appends the literal command to the closing: `run resume` for
  paused/interrupted/checkpoint, `run rerun` for a failed state,
  `run retry-failed-units` / `run retry-unit` for `unit-failed` (a failed
  JOIN is one of those units, so the closing says so, and it names `run
  resume` alongside them: it continues the same attempt, while `run resume
  --phase <id>` is the one form that re-runs work the wave already
  finished),
  `run resolve --approve|--reject` for a `gate` park whose persisted decision
  is a human: route, `run resume` for one whose decision is a park: route
  (D41 amendment — a park: route declares no approve/reject, so naming
  `run resolve` for it would be the dead verb this sentence exists to
  prevent; the kind rides in on `Run.GateDecision` / `Descendant.GateDecision`,
  resolved app-side from the gate trace, and an empty kind names both verbs
  keyed off `run status`'s decision= field), `run answer` for `question`
  (resolve/answer exist since D41; the closing also says a phase session needs
  the `resolve` grant and that the judgment must be the reader's to make), and
  `run resume` for `retries-exhausted` — the turn died on a provider failure the
  transient retries could not outlast, and the session it died in is where the
  next one belongs (`retries-exhausted` IS a `ContinuableReason`) — with
  `run resume --phase <phase-id>` named beside it, because a continuation refills
  no loop budget and the same reason covers a spent loop bound, and
  `run resume` for `stuck` — the phase said what it needs, and once that is
  cleared a bare resume enters the parked phase again as a FRESH attempt
  (`stuck` is not a `ContinuableReason`), with `--refresh-def` named alongside
  it because the usual fix for a stuck phase is an edit to the prompt or the
  definition it froze at start. Every other
  reason prints no verb, because the reason names its
  own cause and a generic "resume" would be exactly the wrong guess. The
  command carries the id of the run being acted on, which for a
  descendant park is the DESCENDANT's, still quoted as untrusted data.
  The states and reasons the closing branches on are mirrored here as
  package constants rather than imported from the engine — this package
  is pure text assembly over a flat input, and importing the engine for a
  handful of strings would drag the whole FSM in.

## Coalescing (K2)

- **Deduplication is by CONTENT, never by a time window.** A timer
  answers the wrong question in both directions: it suppresses a
  genuinely new state that arrives quickly and lets a slow duplicate
  through. What a reader experiences as a duplicate is a message that
  says what the last one said, so `Signature` is over exactly the fields
  that appear in the text — run, resting state, typed reason, phase and
  attempt, the free-text detail, the engine cause, the gate kind, and for
  a descendant park the same set again for the descendant.
- **The digest, the outputs, the references, and the workspace are
  deliberately NOT in the signature.** All of them are derived from the
  coordinate that is: a run resting twice at the same coordinate with a
  fuller record has not asked a second question, it has had its record
  re-read.
- **Free text is bounded in the signature exactly as the composer bounds
  it in the message.** Two causes differing only past `MaxCauseRunes`
  render byte-identical, so treating them as different asks would deliver
  the same words twice.
- **`ProgressSignature` carries the ATTEMPT, and that is load-bearing.**
  A campaign's loop-back notify fires once per wave over the same phase
  and the same route; a signature keyed on route alone would report wave
  one and swallow every wave after it — the failure this mechanism exists
  to prevent, inverted.
- A signature is a readable string rather than a hash because it is
  persisted on the run row (`work_items.wake_signature`, v51+1) and read
  by a human debugging a wake that did or did not arrive. The comparison,
  the record, and the "somebody acted, so the record is spent" clear all
  live app-side in `app_workflow_wake_delivery.go` — this package only
  says what identity means.

## Extension points

- A new reference kind is a new `Reference{Label, Value}` at the call
  site — the composer does not enumerate kinds.
- **A new field that changes the ask goes in the signature too.** The
  two are one decision: if a reader would act differently on the new
  value, two wakes differing only in it are not duplicates. If they
  would not, it is elaboration and stays out.
- **A reference must resolve.** The composer renders whatever it is
  handed, so the rule lives with the resolver
  (`app_workflow_wake.go` → `workflowNarrativeReference`): a narrative
  path is included only when the file is on disk. An agent that opens a
  reference and finds nothing has spent a tool call learning that the
  message was wrong, which is worse than a message with one fewer
  pointer. Same for the descendant's `called run narrative`.
- A new resting state gets a `closing` branch. The default already
  reads correctly for a terminal state, so the branch is about
  precision, not correctness.

## Anti-patterns

- Do NOT reach into the store or the engine from here. If the composer
  needs a new fact, resolve it in `app_workflow_wake.go` and add a
  field to the input.
- Do NOT bypass `untrustedtext` for a field a model can write.
- Do NOT grow a second composer for a new TRIGGER. `ComposeProgress` is
  not one: a resting wake and a progress wake are different *messages* —
  one names a verb the reader owes, the other exists to say no verb is
  owed — and they share every rule and every body writer that they can.
  A new trigger that reports a run resting, or a run continuing, belongs
  in the message that already exists.
- Do NOT make a progress wake reach for a surface a park uses. It is
  inert for an unbound run by design: progress is not an interruption,
  and the OS notification belongs to runs that need a human.

## References

- `docs/specs/workflows-system.md` §5 — thread binding and wake.
- `docs/specs/workflows-system-decisions.md` D17 — the ruling, plus the
  2026-07-25 amendment that made descendant parks surface at the root.
- `app_workflow_wake.go` — resolution.
- `app_workflow_wake_delivery.go` — the one delivery + coalescing
  decision point every composed wake goes through.
- `app_workflow_wake_progress.go` — the progress wake's resolver.
