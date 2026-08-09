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

**An attempt that produced an account has a narrative file**, whatever its access
allowed. There are three sources, in order, and `app_workflow_narrative.go`
(`settleAttemptNarrative`) resolves them behind one existence check and one
`O_EXCL` write, so "an authored account always beats a reconstructed one" is
stated once:

1. the file the element wrote itself — always wins, never touched;
2. the `narrative` control field of its accepted envelope, written verbatim with
   **no** `RecoveredNarrativeHeader`: the element deliberately put it there, so it
   is authored exactly as a file it wrote would be. This is what a `read-only`
   element is now asked for;
3. `RecoverNarrative`, this package's pure fallback for an element that supplied
   neither.

`RecoverNarrative` itself:

- The content is the session's **last** assistant text, prefixed with
  `RecoveredNarrativeHeader` so a reader can tell a reconstructed account from an
  authored one. Earlier texts are not concatenated: the account a phase gives
  before closing out is the one it means.
- The accepted envelope is passed in so the text that IS the envelope can be
  skipped — Codex's structured output arrives as its final message, and
  recovering that JSON as the narrative would be worse than recovering nothing.
  The test is identity with the envelope as a decoded document, never "looks like
  JSON": prose that happens to be JSON is still prose.
- Falling back past that echo is not enough on Codex, whose `outputSchema`
  constrains **every** assistant message of the turn: the earlier texts are
  envelope JSON too. A candidate carrying a top-level `status` is therefore read
  as an envelope (`def.EnvelopeAccount`) and what is lifted is the account it
  holds — its `narrative`, else its `reason` — never its raw JSON. One with no
  account is skipped like the echo. That shape test is deliberately weaker than
  the identity test above, and applies only to candidates that already failed it.
- It reports `false` for a session that said nothing, and the app side never
  overwrites an existing file. Absence stays absence.

## What `PromptSuffix` states, and why

Everything in `<workflow-system-instructions>` is there because the phase would
otherwise get it wrong and the engine could not tell it so afterwards:

- **The narrative** — for human inspection; the system never parses it. **How it
  is asked for follows `access`**, which is the one thing in the suffix that
  varies. A `write` element is given the path and writes the file; a `read-only`
  element runs in a session that denies every file write (D22), so it is asked
  for the narrative in the envelope's `narrative` control field and the path is
  not named at all. The read-only branch asks for a FIELD rather than a message
  because Codex applies a turn's `outputSchema` to every assistant message in it:
  an element under a schema cannot emit prose there, so the old "send it as the
  message immediately before your envelope" was an instruction only Claude could
  follow, and on Codex the D39 fallback recovered a JSON blob as the "narrative".
  The field works identically on both providers, which kills the message leg
  entirely. `BuildPrompt` / `BuildUnitPrompt` derive the access from
  the phase and the unit respectively — a fan-out phase may not declare one, so
  a unit's own declaration is the only correct source — and
  `BuildTakeoverFinalizePrompt` takes it from the caller because a takeover
  steers the element's existing session and inherits its runtime mode.
- **Workspace discipline** (D38) — work in this workspace on its current
  branch; no branch switch, merge, or push unless the prompt says to. A call
  tree shares one branch down the stack (§3a/§9), so a phase that moves it on
  its own initiative moves every later phase's ground. Phrased as a default the
  authored prompt overrides, because a landing phase's whole job is to do this.
- **The commit default**, stated to `write` elements only — phases, work units,
  and joins alike, gated on the same `access` value the narrative branch reads.
  Nothing in the engine ever commits, and everything downstream reads the
  BRANCH rather than the checkout: a later phase resumes on it, a unit's
  sub-worktree is cut from it, a join merges it, and a done join then retires
  the unit worktrees. Without the sentence a writing element can rest on an
  uncommitted tree nothing will ever read, and the retirement destroys it. A
  `read-only` element is told nothing — it has nothing to commit, and the
  instruction would be one more it could not follow. Phrased as a default the
  authored prompt overrides, like the workspace line above.
- **The envelope branch rules**, which `def.ValidateEnvelope` enforces and the
  schema cannot express (D2a). The `question` and `stuck` bullets carry their
  MEANING as well as their mechanics — both park the run for a human, so a
  phase reading `question` as "ask a clarifying question" parks a run that
  should have kept going. `narrative` is stated as being OUTSIDE those rules, on
  both access branches: the schema makes every element answer the field, and an
  unexplained required field is one a phase guesses at. A `read-only` element is
  told it is legal on every status (or a stuck one leaves it null and loses its
  account); a `write` element is told to null it, so the file it authored during
  the work — richer than a field summarized after it — stays the account.

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
- A command may write the optional `narrative` control field too — the schema
  permits it and post-validation is written once against the contract for both
  drivers, so refusing it here would be a second rule set for one contract. It is
  folded into the same file as `ToolReport.Narrative`, leading the process output
  tail (the account is the only part of that file a human did not have to
  reconstruct), and stripped from the envelope at exactly the point the agent
  path strips it.
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
