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
  surface a human or agent actually watches.

## Extension points

- A new reference kind is a new `Reference{Label, Value}` at the call
  site — the composer does not enumerate kinds.
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
