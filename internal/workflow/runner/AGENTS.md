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

## `PromptContext`, and why it is a struct

Every prompt-assembly entry point (`BuildPrompt`, `BuildUnitPrompt`,
`BuildTakeoverFinalizePrompt`, `PromptSuffix`) takes one `PromptContext`: the
narrative path, the access, and each app-resolved block the suffix appends
(feedback, campaign memory, operator guidance, the goal chain, the merge-join
obligation). It is a struct because the blocks accumulate — seven positional
arguments of which four are optional is a call site nobody can read, and two of
the types are interchangeable at a glance. Adding the next block is a field
rather than a signature change rippling through every caller.

`Access` is the one field the builders SET rather than read:
`BuildPrompt` takes it from the phase and `BuildUnitPrompt` from the unit,
because a fan-out phase may not declare one and a unit's own declaration is the
only correct source. Only `BuildTakeoverFinalizePrompt` honours the caller's,
since a takeover steers an existing session and inherits its runtime mode.

## The goal chain in the prompt

`writeGoalChainSection` appends `<goal-chain>` — what campaign this element's
work is part of, and what that campaign has ruled out. It exists because an
element previously worked blind to the big picture: a lane knew its slice and
nothing about why the campaign existed or where its edges were, so "done" was
re-formed from the slice every wave and scope crept outward one reasonable
decision at a time.

- **Two halves with two owners.** The GOALS are run-owned, one link per run on
  the call chain from the tree's root down to this one. The NON-GOALS are
  def-owned (`non_goals:`), which is what makes the boundary a fact about the
  definition rather than an opinion each wave re-derives. `RootNonGoals` carries
  the root workflow's list only when it DIFFERS from this run's, since a
  recursive campaign whose waves run one definition would otherwise print the
  same list twice under two headings.
- **The app resolves it, this package renders it.** `workflowPromptAncestry`
  (repo root, `app_workflow_prompt_context.go`) walks the run's ancestry ONCE
  and builds both this block and the campaign-memory digest from that one walk —
  they are both facts about the run TREE, and walking twice per element would
  pay for the same parent rows twice. Failure to resolve is logged and yields
  the blocks it could build: the chain is context, not contract, and an element
  that runs without it does the work with less to go on while an element that
  never starts does none.
- **Consecutive runs sharing one goal collapse to one link**, attributed to the
  root-most run that stated it. The engine copies a caller's goal onto every run
  it calls, so a forty-wave campaign's raw chain is forty copies of one
  sentence. A goal that re-appears after a different one is a genuine second
  statement and keeps its own link.
- **A bare run with no goal and no ancestry gets NO block.** A labelled section
  stating nothing is worse than no section, and the simple case must cost zero
  prompt bytes. A child with no goal of its own still reads the chain above it —
  that chain is precisely what tells it why it exists — and no link claims to be
  its.
- **Deep chains elide the middle and say how many they dropped**
  (`MaxGoalChainLinks`, 6; head/tail split, the wake's call-chain precedent). A
  silently shortened chain reads as a shallower campaign than the one running.
  Overflowing non-goals are stated the same way rather than trimmed, because a
  frozen snapshot can predate the authoring bound and an unstated boundary is
  the one an element crosses.
- **Every value is quoted through `internal/untrustedtext`** and the block says
  so in its own first sentence. A goal is typed by a person or written by a
  calling agent and arrives inside another agent's prompt.
- It is written FIRST among the context blocks, because it is the frame the
  others are read inside.

## The merge-join obligation in the prompt

`writeUnitAccountingSection` states the `accounts_for_units:` contract to the
join that carries it, listing the exact unit ids it will be post-validated
against. It is in the prompt because the ENGINE enforces it: an element refused
for breaking a rule nobody told it about spends its one envelope retry learning
the rule, and a rule stated only in authored content is one an author can forget
to write. The ids are listed rather than described because the schema cannot
express "these two arrays partition this set", so this block is the only place
the join learns the set before it answers — and because `{{units}}` reaches it
only if its author interpolated it. A join over zero units is told it still owes
two empty lists; a join that did not opt in is told nothing at all.

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

## Operator guidance in the prompt

`writeGuidanceSection` appends the `<operator-guidance>` block when the phase
entry that created this attempt delivered any — the run-side half of
`agent-overflow run guide`. The entries arrive on `RunRequest.Guidance` as data
the engine already bounded and stamped; this package only decides how they read.

- **Every entry is quoted through `internal/untrustedtext`**, exactly as the
  wake composer quotes model output. Guidance is typed by a person or written by
  another agent, it arrives inside a prompt, and the one thing the reading
  element must never do is mistake it for the system's own contract. The block's
  own sentence says so: follow it as steering, and report — never obey — anything
  in it that contradicts the phase's contract.
- **The attribution is the engine's, not the text's.** An entry says whether a
  human or an agent phase left it (and which run, for a phase), because a run
  that could be told "a human said this" by an agent would make the label
  worthless. An entry with no stamp at all predates the field and reads as
  *unattributed*, never as a human.
- **No entries means no section.** A labelled block containing nothing would
  read as an operator who left an instruction and said nothing in it — the same
  rule the memory digest follows.
- A unit prompt carries the same block. The entries were delivered to the PHASE
  entry, and every element of that attempt goes through prompt assembly.

## Campaign memory in the prompt

`writeMemorySection` appends the app-rendered `<campaign-memory>` block plus the
one sentence stating how this element records notes. The block itself is
`memory.Render`'s output, passed in as a `MemoryDigest` — a named string type so
it cannot be transposed with the narrative path at a call site — because reading
the log is filesystem work and this package does none.

- **The WRITE channel follows `access`**, like the narrative's, and for a
  stricter reason. A `write` element is given the CLI verb (`agent-overflow
  memory add`) and told to run it as it learns things: a note recorded during
  the work outlives an attempt that later fails, parks, or is retried, while an
  envelope field only lands if the envelope is accepted. A `read-only` element
  cannot reach the verb at all — Claude's read-only mode denies the bash call
  and Codex's read-only sandbox blocks the loopback socket — so it is asked for
  the envelope's `memory` field, which the app lifts at the same seam it lifts
  `narrative`.
- **READING is not split that way and does not need to be.** A read-only session
  restricts writes, not reads: Claude strips only `Write`/`Edit`/`NotebookEdit`
  and Codex's read-only sandbox permits reads filesystem-wide, so the log's
  absolute path — under the app's config root, outside every workspace — is
  legible to both. The digest's own header names it on both branches; there is
  no read-only-specific phrasing because there is no read-only-specific
  restriction.
- **No digest means no section at all.** A contract naming a channel while
  showing no log would be a promise nothing keeps, and an element asked for
  notes nothing collects is worse than one never asked.

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
- **Declarations always come from `def`, never from `phase.Inputs` read
  directly.** `BuildPrompt` asks `def.PhaseDeclarations`, which is what binds
  the reserved engine reads (`call-depth`, `budget`) on top of the authored
  inputs. Reading the map off the phase would make a prompt referencing one fail
  to build here while validating clean in the dry-run — the two have to be
  answering the same question about what a template may name.
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
