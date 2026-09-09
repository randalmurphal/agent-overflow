# Workflow prompt and tool runner helpers

This package builds agent prompts and executes profile-defined tool commands for
one workflow element. Provider sessions, persistence, filesystem ownership, and
worktree provisioning belong to `internal/workflowhost` and the application.

## Prompt contracts

- Inputs are already-resolved workflow values. Do not perform definition
  interpolation or routing here.
- Keep system-owned context separate from the authored prompt. The suffix states
  envelope fields, branch rules, output declarations, workspace discipline, and
  the commit expectation for write access.
- Render the goal chain from app-resolved ancestry. Collapse consecutive runs
  sharing a goal, bound deep chains by eliding the middle, and omit the block
  when there is no goal context.
- Quote every model- or user-supplied value with `internal/untrustedtext` before
  embedding it in a system block.
- Guidance entries retain engine attribution and appear only when nonempty.
  Unit prompts receive the phase guidance selected by the engine.
- Workflow-memory context is readable by every access mode. Only write-capable
  elements receive the command for adding notes. Omit an empty memory digest.
- Fan-out declarations come from `def` contracts, not directly from raw phase
  inputs. Unit attempt paths remain nested under the phase attempt.
- For merge joins with `accounts_for_units`, state the accounting obligation in
  the prompt and rely on envelope validation for enforcement.

## Narrative recovery

- Recover narrative from the session's last assistant text and skip text equal
  to the accepted envelope.
- For Codex structured sessions, recover the useful pre-envelope assistant text
  according to the provider transcript contract.
- Return no narrative when the session contains none; callers must not invent a
  placeholder artifact.

## Tool execution

- Resolve command argv from the live project profile at phase start.
- Run in the prepared workspace with resolved profile secrets and the standard
  `AO_*` contract.
- Valid JSON written to `AO_ENVELOPE` is authoritative and passes through the
  same envelope validation as an agent result.
- A nonzero command exit is a check result (`passed: false`); launch, I/O,
  timeout, and malformed-envelope failures are execution errors.
- Bound combined output by retaining its tail and persist it through the
  attempt-output path.
- The inactivity watchdog measures output bytes and must remain distinct from
  an overall execution timeout.

See `docs/specs/workflows-system.md` for prompt and tool-driver semantics.
