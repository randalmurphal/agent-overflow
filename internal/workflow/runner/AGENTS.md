# internal/workflow/runner/

Pure helper functions for app-owned workflow phase execution.

## Boundaries

- Prompt interpolation, the system-owned suffix, attempt path construction,
  envelope-to-outcome mapping, tool-envelope synthesis/overlay, tool narrative
  rendering, and validation retry messages live here.
- Provider sessions, App state, observers, persistence, and filesystem writes
  stay in the main package.
- Inputs are already-resolved runtime workflows: phase prompts are bodies, not
  authored file paths.
- The app-owned runner provisions writing-item worktrees and setup hooks before
  the first phase, reuses persisted workspaces, and captures declared artifacts
  before reporting a successful phase completion. Cleanup remains inert until
  disposition support lands; unlanded worktrees are never discarded.

## Narrative recovery (D39)

`RecoverNarrative` is the pure half of the guarantee that **an attempt that
produced an account has a narrative file**, whatever its access allowed:

- The app runner calls it when an agent turn's envelope is accepted (`done`,
  `question`, or `stuck`) and the narrative file is absent — see
  `app_workflow_narrative.go`. It applies to phases, fan-out units, and joins
  alike, because all three own a narrative path.
- The content is the session's **last** assistant text, prefixed with
  `RecoveredNarrativeHeader` so a reader can tell a reconstructed account from an
  authored one. Earlier texts are not concatenated: the account a phase gives
  before closing out is the one it means.
- The accepted envelope is passed in so the text that IS the envelope can be
  skipped — Codex's structured output arrives as its final message, and
  recovering that JSON as the narrative would be worse than recovering nothing.
  The test is identity with the envelope as a decoded document, never "looks like
  JSON": prose that happens to be JSON is still prose.
- It reports `false` for a session that said nothing, and the app side never
  overwrites an existing file. Absence stays absence, and an authored narrative
  always wins over a reconstructed one.

## What `PromptSuffix` states, and why

Everything in `<workflow-system-instructions>` is there because the phase would
otherwise get it wrong and the engine could not tell it so afterwards:

- **The narrative** — for human inspection; not part of the envelope. **How it
  is asked for follows `access`**, which is the one thing in the suffix that
  varies. A `write` element is given the path and writes the file; a `read-only`
  element runs in a session that denies every file write (D22), so it is asked
  for the narrative as the message immediately before its envelope and the path
  is not named at all. `BuildPrompt` / `BuildUnitPrompt` derive the access from
  the phase and the unit respectively — a fan-out phase may not declare one, so
  a unit's own declaration is the only correct source — and
  `BuildTakeoverFinalizePrompt` takes it from the caller because a takeover
  steers the element's existing session and inherits its runtime mode.
- **Workspace discipline** (D38) — work in this workspace on its current
  branch; no branch switch, merge, or push unless the prompt says to. A call
  tree shares one branch down the stack (§3a/§9), so a phase that moves it on
  its own initiative moves every later phase's ground. Phrased as a default the
  authored prompt overrides, because a landing phase's whole job is to do this.
- **The envelope branch rules**, which `def.ValidateEnvelope` enforces and the
  schema cannot express (D2a). The `question` and `stuck` bullets carry their
  MEANING as well as their mechanics — both park the run for a human, so a
  phase reading `question` as "ask a clarifying question" parks a run that
  should have kept going.

## Fan-out units

- A unit's app-managed files nest under its phase attempt's directory
  (`UnitAttemptDir` → `<phase>.<attempt>/units/<unit>.<try>`), so a run stays
  one tree per attempt. The try number is in the path because a retried unit
  reuses its row but must not reuse its narrative: the previous try's account of
  what it did is evidence a human retries or drops on, not scratch space.
  `UnitNarrativePath` / `UnitEnvelopePath` are the unit-scoped counterparts of
  `NarrativePath` / `EnvelopePath` and refuse any id that would not stay one
  path segment down.
- `BuildUnitPrompt` takes its declarations from the caller rather than deriving
  them, because they differ by role: a work unit reads the phase's inputs plus
  its `as:` element binding (`def.ResolveUnitDeclarations`), a join reads the
  phase's inputs plus the reserved `units` results (`def.JoinDeclarations`).
  Both then get the same system-owned suffix — narrative instruction, feedback
  block, and envelope branch rules — as a phase prompt.
- `ToolReport` carries `UnitID`/`UnitAttempt` so a tool unit's narrative names
  the unit it belongs to and its envelope guidance says "unit" where a phase's
  says "phase". Everything else about the tool contract below is identical for
  a unit, a join, and a phase.

## Tool-driver execution contract

`app_workflow_tool.go` owns the process; this package owns the pure shape of
what that process produces. The contract both sides implement:

- Argv comes from the **live** project profile at phase start — `checks[<name>]`
  for `check:`, `commands[<name>]` for `command:` — as an argv array, never a
  shell string. Every element goes through the same interpolation the agent
  driver applies to prompts. A missing or unresolvable binding is
  `engine.ErrWiringFailed`, not a retryable attempt.
- The process runs in the phase's workspace with resolved profile secrets in its
  environment (`ResolvedSecrets.Environ`) and `AO_ENVELOPE` set to an absolute
  path inside the attempt directory (`EnvelopePath`).
- If the command writes valid JSON to `AO_ENVELOPE`, that **is** the phase
  envelope and goes through `def.ValidateEnvelope` exactly like an agent's.
  `ApplyToolOutputs` then overlays `passed` and `exit-code` onto a `done`
  envelope, because a command cannot know its own exit status while writing the
  file. If it writes nothing, `SynthesizedToolEnvelope` produces
  `{status: done, outputs: {passed, exit-code, <optional authored>: null}}` —
  optional authored outputs are filled with an explicit null, required ones are
  never invented, so a phase that declares one fails post-validation instead of
  advancing on a fabricated contract.
- A non-zero exit is `passed: false`, not a phase failure; the gate decides.
  Infra failures are typed: binding/interpolation → `ErrWiringFailed`, secret
  resolution / workspace provisioning / process start → `ErrSetupFailed`,
  envelope-production failure → execution failure with the findings recorded
  (a deterministic command cannot be retried into validity, so it parks through
  the existing exhaustion path with no retry attempt).
- Combined stdout+stderr is tail-capped and persisted through the existing
  per-attempt **narrative** file (`ToolNarrative`), masked with the profile's
  secret masks. Tool attempts hold no provider session, so their
  `work_item_phases` row has an empty `thread_id`. No new store table exists for
  tool output.
- The profile inactivity watchdog measures output bytes. No bytes for the
  window means the process group is killed and the run parks
  `needs-human(stalled)`; cancel, pause, and shutdown kill through that same
  single path. The reaping goroutine is the only place a tool outcome is
  reported and it always writes the narrative first.
