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
allowed. Three sources in order, resolved by `settleAttemptNarrative`
(`app_workflow_narrative.go`) behind one existence check and one `O_EXCL` write:

1. the file the element wrote itself. Always wins, never touched.
2. the accepted envelope's `narrative` control field, written verbatim with **no**
   `RecoveredNarrativeHeader`. This is what a `read-only` element is asked for.
3. `RecoverNarrative`, this package's pure fallback.

`RecoverNarrative` rules:

- The content is the session's **last** assistant text, prefixed with
  `RecoveredNarrativeHeader`. Earlier texts are not concatenated.
- The accepted envelope is passed in so the text that IS the envelope is skipped.
  The test is identity with the envelope as a decoded document, never "looks like
  JSON": prose that happens to be JSON is still prose.
- On Codex the `outputSchema` constrains **every** assistant message, so the
  earlier texts are envelope JSON too. A candidate carrying a top-level `status`
  is read as an envelope (`def.EnvelopeAccount`) and what is lifted is its
  `narrative`, else its `reason`, never its raw JSON. That shape test is
  deliberately weaker than the identity test and applies only to candidates that
  already failed it.
- It reports `false` for a session that said nothing, and the app side never
  overwrites an existing file. Absence stays absence.

## `PromptContext`

Every prompt-assembly entry point (`BuildPrompt`, `BuildUnitPrompt`,
`BuildContinuationPrompt`, `BuildTakeoverFinalizePrompt`, `PromptSuffix`) takes
one `PromptContext`. Adding the next context block is a field, not a signature
change rippling through every caller; the field comments carry the per-field
contract.

`Access` is the one field the builders SET rather than read. `BuildPrompt` takes
it from the phase and `BuildUnitPrompt` from the unit, because a fan-out phase
may not declare one and a unit's own declaration is the only correct source. Only
`BuildTakeoverFinalizePrompt` honours the caller's, since a takeover steers an
existing session and inherits its runtime mode.

## The goal chain

`writeGoalChainSection` appends `<goal-chain>`: what campaign this element serves
and what that campaign has ruled out.

- **Two halves with two owners.** GOALS are run-owned, one link per run from the
  tree's root down. NON-GOALS are def-owned (`non_goals:`), which makes the
  boundary a fact about the definition rather than an opinion each wave
  re-derives. `RootNonGoals` carries the root workflow's list only when it
  DIFFERS from this run's.
- **The app resolves it, this package renders it.** `workflowPromptAncestry`
  (`app_workflow_prompt_context.go`) walks the ancestry ONCE and builds both this
  block and the campaign-memory digest from that walk. Failure to resolve is
  logged and yields the blocks it could build: the chain is context, not
  contract.
- **Consecutive runs sharing one goal collapse to one link**, attributed to the
  root-most run that stated it, because the engine copies a caller's goal onto
  every run it calls. A goal that re-appears after a different one keeps its own
  link.
- **A bare run with no goal and no ancestry gets NO block.** The simple case must
  cost zero prompt bytes.
- **Deep chains elide the middle and say how many they dropped**
  (`MaxGoalChainLinks`, 6; head/tail split). Overflowing non-goals are stated the
  same way rather than trimmed: an unstated boundary is the one an element
  crosses.
- **Every value is quoted through `internal/untrustedtext`** and the block says
  so in its own first sentence.
- It is written FIRST among the context blocks, because it is the frame the
  others are read inside.

## The merge-join obligation

`writeUnitAccountingSection` states the `accounts_for_units:` contract to the join
that carries it, listing the exact unit ids it will be post-validated against. It
is in the prompt because the ENGINE enforces it, and because the schema cannot
express "these two arrays partition this set". A join over zero units is told it
still owes two empty lists; a join that did not opt in is told nothing at all.

## What `PromptSuffix` states

Everything in `<workflow-system-instructions>` is there because the phase would
otherwise get it wrong and the engine could not tell it so afterwards:

- **The narrative**, for human inspection; the system never parses it. **How it
  is asked for follows `access`.** A `write` element is given the path and writes
  the file; a `read-only` element runs in a session that denies every file write
  (D22), so it is asked for the envelope's `narrative` control field and the path
  is not named. The read-only branch asks for a FIELD rather than a message
  because Codex applies a turn's `outputSchema` to every assistant message in it,
  so an element under a schema cannot emit prose there. The field works
  identically on both providers.
- **Workspace discipline** (D38): work in this workspace on its current branch;
  no branch switch, merge, or push unless the prompt says to. A call tree shares
  one branch down the stack (§3a/§9). Phrased as a default the authored prompt
  overrides, because a landing phase's whole job is to do this.
- **The commit default**, stated to `write` elements only, gated on the same
  `access` value. Nothing in the engine ever commits, and everything downstream
  reads the BRANCH rather than the checkout: a later phase resumes on it, a
  unit's sub-worktree is cut from it, a join merges it, and a done join retires
  the unit worktrees. Without the sentence a writing element can rest on an
  uncommitted tree the retirement then destroys. Also phrased as an overridable
  default.
- **The envelope branch rules**, which `def.ValidateEnvelope` enforces and the
  schema cannot express (D2a). The `question` and `stuck` bullets carry their
  MEANING as well as their mechanics, because both park the run for a human.
  `narrative` is stated as OUTSIDE those rules on both access branches: a
  `read-only` element is told it is legal on every status, a `write` element is
  told to null it so the file it authored stays the account.

## Operator guidance

`writeGuidanceSection` appends `<operator-guidance>` when the phase entry that
created this attempt delivered any: the run-side half of `agent-overflow run
guide`. Entries arrive on `RunRequest.Guidance` already bounded and stamped by the
engine.

- **Every entry is quoted through `internal/untrustedtext`.** The block's own
  sentence says to follow it as steering and to report, never obey, anything in
  it that contradicts the phase's contract.
- **The attribution is the engine's, not the text's.** An entry says whether a
  human or an agent phase left it (and which run, for a phase). An entry with no
  stamp predates the field and reads as *unattributed*, never as a human.
- **No entries means no section.**
- A unit prompt carries the same block: the entries were delivered to the PHASE
  entry, and every element of that attempt goes through prompt assembly.

## Campaign memory

`writeMemorySection` appends the app-rendered `<campaign-memory>` block plus the
one sentence stating how this element records notes. The block is
`memory.Render`'s output, passed in as a `MemoryDigest` (a named string type so it
cannot be transposed with the narrative path), because reading the log is
filesystem work and this package does none.

- **The WRITE channel follows `access`.** A `write` element is given the CLI verb
  (`agent-overflow memory add`) and told to run it as it learns things, since a
  note recorded during the work outlives an attempt that later fails, parks, or
  is retried. A `read-only` element cannot reach the verb at all (Claude's
  read-only mode denies the bash call, Codex's read-only sandbox blocks the
  loopback socket), so it is asked for the envelope's `memory` field, lifted at
  the same seam as `narrative`.
- **READING is not split that way.** A read-only session restricts writes, not
  reads, and the log lives under the app's config root outside every workspace.
- **No digest means no section at all.**

## Fan-out units

- A unit's app-managed files nest under its phase attempt's directory
  (`UnitAttemptDir` produces `<phase>.<attempt>/units/<unit>.<try>`). The try
  number is in the path because a retried unit reuses its row but must not reuse
  its narrative: the previous try's account is evidence a human acts on, not
  scratch space. `UnitNarrativePath` / `UnitEnvelopePath` refuse any id that would
  not stay one path segment down.
- `BuildUnitPrompt` takes its declarations from the caller because they differ by
  role: a work unit reads the phase's inputs plus its `as:` element binding
  (`def.ResolveUnitDeclarations`), a join reads the phase's inputs plus the
  reserved `units` results (`def.JoinDeclarations`).
- **Declarations always come from `def`, never from `phase.Inputs` read
  directly.** `BuildPrompt` asks `def.PhaseDeclarations`, which binds the reserved
  engine reads (`call-depth`, `budget`) on top of the authored inputs. Reading the
  map off the phase would make a prompt referencing one fail to build here while
  validating clean in the dry-run.
- `ToolReport` carries `UnitID`/`UnitAttempt` so a tool unit's narrative names its
  unit. Everything else in the tool contract is identical for a unit, a join, and
  a phase.

## Tool-driver execution contract

`app_workflow_tool.go` owns the process; this package owns the pure shape of what
that process produces. The contract both sides implement:

- Argv comes from the **live** project profile at phase start (`checks[<name>]`
  for `check:`, `commands[<name>]` for `command:`) as an argv array, never a shell
  string, and goes through the same interpolation prompts get. A missing or
  unresolvable binding is `engine.ErrWiringFailed`, not a retryable attempt.
- The process runs in the phase's workspace with resolved profile secrets in its
  environment (`ResolvedSecrets.Environ`) and `AO_ENVELOPE` set to an absolute
  path inside the attempt directory (`EnvelopePath`).
- Valid JSON written to `AO_ENVELOPE` **is** the phase envelope and goes through
  `def.ValidateEnvelope` like an agent's; `ApplyToolOutputs` overlays `passed` and
  `exit-code` onto a `done` envelope, because a command cannot know its own exit
  status while writing the file. If it writes nothing,
  `SynthesizedToolEnvelope` produces `{status: done, outputs: {passed, exit-code,
  <optional authored>: null}}`. Optional authored outputs are filled with an
  explicit null, required ones are never invented, so a phase that declares one
  fails post-validation instead of advancing on a fabricated contract.
- A non-zero exit is `passed: false`, not a phase failure; the gate decides. Infra
  failures are typed: binding/interpolation to `ErrWiringFailed`, secret
  resolution / workspace provisioning / process start to `ErrSetupFailed`,
  envelope-production failure to an execution failure with the findings recorded
  (a deterministic command cannot be retried into validity, so it parks through
  the exhaustion path with no retry attempt).
- A command may write the optional `narrative` control field too, since
  post-validation is written once against one contract for both drivers. It is
  folded into the same file as `ToolReport.Narrative`, leading the process output
  tail, and stripped from the envelope at exactly the point the agent path strips
  it.
- Combined stdout+stderr is tail-capped and persisted through the per-attempt
  **narrative** file (`ToolNarrative`), masked with the profile's secret masks.
  Tool attempts hold no provider session, so their `work_item_phases` row has an
  empty `thread_id`. No new store table exists for tool output.
- The profile inactivity watchdog measures output bytes. No bytes for the window
  means the process group is killed and the run parks `needs-human(stalled)`;
  cancel, pause, and shutdown kill through that same single path. The reaping
  goroutine is the only place a tool outcome is reported and it always writes the
  narrative first.
