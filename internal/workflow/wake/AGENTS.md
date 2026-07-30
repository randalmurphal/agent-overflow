# internal/workflow/wake/

The compact message a resting root run injects into its bound thread
(spec §5, decision D17). Pure text assembly over a flat, pre-resolved
`Input`; the app layer resolves the run record and owns delivery.

## Invariants

- **Pure.** `Compose` performs no lookups and no I/O. The same `Input`
  always produces the same message, which is what makes the format
  testable without a store, an engine, or a provider.
- **No envelope dumps.** A wake names the resting state, its typed
  reason, the phase's free text, the run's *declared* outputs, and
  references. Raw envelopes, gate traces, and diffs are reachable
  through the references — they never ride the message.
- **Every quoted value is data.** Goals, questions, stuck reasons, and
  output values come out of a model. They go through
  `internal/untrustedtext` and the message leads with the notice that
  says so, because the reader is another agent.
- **Bounded.** Outputs, references, and every free-text field carry a
  rune/count budget with an explicit "…and N more" tail. A run that
  produced a thousand outputs still composes a message a thread can
  take.
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
  a resolution.
- **A closing names the verb, not just the run (D38).** `repairSentence`
  appends the literal command to the closing: `run resume` for
  paused/interrupted/checkpoint, `run rerun` for a failed state,
  `run retry-failed-units` / `run retry-unit` for `unit-failed`, and for
  `gate`/`question` the fact that only a human decides them — no CLI verb
  does. Every other reason prints no verb, because the reason names its
  own cause and a generic "resume" would be exactly the wrong guess. The
  command carries the id of the run being acted on, which for a
  descendant park is the DESCENDANT's, still quoted as untrusted data.
  The states and reasons the closing branches on are mirrored here as
  package constants rather than imported from the engine — this package
  is pure text assembly over a flat input, and importing the engine for a
  handful of strings would drag the whole FSM in.

## Extension points

- A new reference kind is a new `Reference{Label, Value}` at the call
  site — the composer does not enumerate kinds.
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
  field to `Input`.
- Do NOT bypass `untrustedtext` for a field a model can write.
- Do NOT grow a second composer for a new trigger. One message shape,
  one place to change it — that is the whole point of the package.

## References

- `docs/specs/workflows-system.md` §5 — thread binding and wake.
- `docs/specs/workflows-system-decisions.md` D17 — the ruling, plus the
  2026-07-25 amendment that made descendant parks surface at the root.
- `app_workflow_wake.go` — resolution and delivery.
