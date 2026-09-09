# Workflow wake message composition

This package purely composes bounded messages for a resting run and for a gate
progress notification. Application code resolves store and filesystem facts,
chooses a delivery surface, and persists coalescing state.

## Message contracts

- Never include a raw envelope. Render the run state, typed reason, bounded
  detail, engine diagnosis, outputs, references, and the exact next command the
  recipient can take.
- Pass every model- or user-supplied value through `internal/untrustedtext`.
- Apply explicit rune and item bounds to all free text and repeated fields.
- A descendant park is reported on the bound root surface. Identify the parked
  descendant, bounded call chain, descendant attempt outputs, and the command
  that targets that descendant. Do not present unfinished root outputs as
  descendant results.
- A checkpoint is an expected stop and directs the recipient to resume.
- Keep repair commands aligned with engine semantics for pause, interruption,
  checkpoint, failure, failed units and joins, human gates, questions, provider
  failures and usage limits, loop exhaustion, and stuck phases.
- A progress notification says that execution continued and creates no repair
  obligation or OS-level attention for an unbound run.

## Coalescing identity

- `Signature` identifies rendered meaning, not elapsed time. Include every field
  whose change would alter the recipient's required action.
- Bound signature text exactly as composition bounds it. Byte-identical rendered
  asks must have the same signature.
- Include attempt identity in progress signatures so later loop attempts are not
  suppressed.
- Exclude derived elaboration such as resolved paths and output snapshots when
  it does not change the action requested.
- Signatures are readable persisted values. Keep their format stable unless the
  delivery migration accounts for existing rows.
- Delivery records a signature only after the message reaches its durability
  point. Live-session queue delivery uses a `queued:<signature>` claim and
  compare-and-set promotion so an intervening action can spend the pending
  notification.
- A queued claim never suppresses a real signature. Prefer a duplicate after a
  crash to permanently losing the only wake.

If a new input changes what the recipient should do, add it to both composition
and signature identity. Keep lookup, reference existence checks, delivery, and
provider-usage correlation in `internal/workflowapp`.

See `docs/specs/workflows-system.md` for binding and wake behavior and
`docs/specs/workflows-system-decisions.md` for descendant surfacing.
