# ADR-009: Stop Is a Turn Interrupt, Not a Per-Tool Kill

Status: accepted
Date: 2026-04-18

## Context

When the user clicks "Stop" during a running turn, what happens? Two
models:

- **Per-tool kill.** Stop targets the specific running tool (or
  whichever tool is visibly active). Kill that tool; let any
  follow-up tools in the same turn also die. The assistant text
  remains.
- **Turn interrupt.** Stop ends the current turn. Every
  streaming / running item in the turn flips to errored; a
  synthetic "Stopped by user" row is persisted at the end of the
  turn. The provider process receives an interrupt signal.

Per-tool kill is finer-grained but has nasty edge cases:

- If the user stops one tool and the turn has more tools pending,
  which ones run? None? Only some? Undefined.
- Assistant text generated between tool calls becomes orphaned.
- The provider doesn't understand "stop this tool but keep going".
  That's not a primitive the wire protocol exposes.

## Decision

Stop is a turn-level interrupt. Clicking Stop:

1. Calls `Session.Stop` on the provider, which sends the
   provider-native interrupt primitive (Claude: close stdin; Codex:
   `turn/interrupt`).
2. Triage flips every streaming/running item in the current turn to
   `errored` and appends the summary " — stopped".
3. Drains the interrupt queue as errored so deferred rows carry the
   same status.
4. Appends a synthetic `error` item with summary "Stopped by user"
   at the end of the turn.

## Rationale

- **Maps to provider primitives.** Both providers expose a
  turn-level interrupt; neither exposes per-tool kill. The chosen
  behavior is what the underlying protocol supports.
- **Clear user model.** "Stop means stop the turn" is unambiguous.
  "Stop means stop this one thing" invites "which thing?" follow-up
  questions.
- **Matches t3-code, Claude CLI.** The reference UXes we track all
  use turn-level interrupt. Users moving between tools expect the
  same behavior.
- **Preserves history.** Errored items remain on the timeline so the
  user can see what was interrupted. The "Stopped by user" marker
  makes the turn's end state explicit.

Considered alternatives:

- **Per-tool kill.** Rejected: not a provider primitive; opens
  orphaned-state questions we can't answer.
- **Hybrid (stop current tool, continue turn).** Rejected: same
  orphan problem; provider doesn't support it.

## Consequences

- The distinction between "user stopped" and "turn truncated by
  provider error" is preserved: `stoppedSummary` appends
  " — stopped" (user-initiated); `interruptedSummary` appends
  " — interrupted" (provider-initiated). Both flow through the
  same `flipTurnItemsErrored` helper in
  `internal/triage/turn_lifecycle.go`.
- The synthetic "Stopped by user" row uses a deterministic id
  (`error:<turn_index>:<seq>`) so a resumed session that re-emits
  the turn can upsert-replace rather than duplicate the marker.
- The interrupt queue drains on stop. Deferred events after the
  stop don't land as completed; they land as errored.
- Stop drains pending approval and user-input requests for the
  interrupted turn. There is no app-side approval timeout: prompts stay
  open until the user responds, an explicit stop/interrupt/provider
  cancel resolves them as `cancel` where applicable, or session/provider
  death resolves them as `lost`.
