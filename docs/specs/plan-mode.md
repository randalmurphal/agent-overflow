# Plan mode

Status: design signed off in conversation on 2026-09-27, revised after the
2026-09-27 design review. Not implemented. Replaces the provider plan modes and the proposed-plan machinery
described under [Removal](#removal-of-the-provider-plan-modes).

## Goal

Produce a merge-ready result from what the user asked for, while the user
spends the least attention that still lets them make every decision that
is theirs. The agent owns facts and technical detail and never asks for
them. The user owns intent and the agent never infers it. Nothing the
user did not ask for reaches them.

Plan mode is the whole run, not the planning phase: nail the intent,
plan, build in phases, validate to the project's definition of done,
place the durable context in the repository, declare done with evidence.

## Why the provider modes go

Claude's plan mode is a permission mode. In a headless session it blocks
every non-read-only action regardless of the thread's access tier: from
Claude Code 2.1.278 the bypass carve-out applies only to interactive
terminals, and the docs state that plan mode "keeps its blocks wherever
Claude Code runs without an interactive terminal". Codex's plan mode is a
per-turn collaboration mode: it carries mode, model and effort, leaves
approval and sandbox policy alone, and its template ends in a
`<proposed_plan>` block. Observed on both: the agent's output converges
on the exit artifact rather than on the decisions the user still owns.
The behavior this spec wants is a protocol and an artifact and needs no
provider mode.

Spike evidence (2026-09-25, Claude 2.1.257 and 2.1.280, Codex 0.155.1):

- Headless bypass session switched to `plan`: 2.1.257 auto-allows Bash,
  Read outside the workspace and edits; 2.1.280 sends `can_use_tool` for
  each. `--allow-dangerously-skip-permissions` and `apply_flag_settings`
  allow rules do not change this.
- `EnterPlanMode` is loadable through ToolSearch and switches a bypass
  session into `plan` with no prompt; `--disallowedTools EnterPlanMode
  ExitPlanMode` removes both tools entirely.
- `--permission-mode` omitted for the approval-required tier lets the
  user's `permissions.defaultMode` setting choose the mode; an explicit
  `--permission-mode default` overrides it.
- Codex `collaborationMode` omitted on `turn/start` keeps the mode the
  last turn set in that process; a resumed thread starts in `default`
  regardless of its last turn. `collaborationMode.settings` requires
  `model`.

## Removal of the provider plan modes

- The chat/plan interaction choice is removed for every provider. The
  manual selection (`ValidateCreate`, `ValidateSet`, drafts,
  `thread_spawn`, peer calls) accepts `chat` only. The `mode` column keeps
  its other values (discussion, terminal, workflow, scratch, holder).
  `plan` from an older peer, an older transfer manifest or the closed
  thread-tool decoder is accepted and coerced to `chat` at that
  boundary, never rejected.
- Headless Claude sessions always pass `--permission-mode` for the
  thread's tier, including `default` for approval-required, and always
  pass `--disallowedTools EnterPlanMode ExitPlanMode`. `set_permission_mode
  "plan"` is never sent. The Claude TUI provider keeps its own launch
  contract and is out of scope for the tier flag. The user's own
  `defaultMode` setting is their concern beyond passing the tier
  explicitly.
- Codex turns stop sending `collaborationMode`. Model and effort already
  ride the top-level `turn/start` fields.
- The proposed-plan write paths go: plan comments, revision send,
  implement-plan send, pending-implementation drafts, the `Plan ready`
  state and the `proposed_plans` and `proposed_plan_comments` tables.
  The `plan-ready` `thread_search` state and the `mode` parameter of
  `thread_spawn` go with them. The diff-review constants that reuse the
  proposed-plan constants get their own owner.
- Stored `proposed_plan` items stay immutable and render as a read-only
  card with Copy and Save. Plan items still arrive from Codex rollout
  import, from the Claude TUI provider and from Codex itself; the live
  handler keeps persisting the body and stops creating state. Transfer
  bundles from older peers still carry `plan` and `plan_comment`
  records; the importer accepts and discards them.
- One forward migration with frozen current SQL rewrites
  `threads.mode = 'plan'` and `scratch_threads.return_mode = 'plan'` to
  `chat`, clears `thread_drafts.pending_plan_implementation` and
  recomputes `has_content`, strips `sourceProposedPlan`,
  `revisionSourceProposedPlan` and `revisionSourceCommentIds` from queued
  `flush_queue_items` and drops a queued item whose only content was the
  comments, drops the two tables, and restamps every item that carried
  plan decoration through the history revision contract
  (`history_sync.go`), so a held window whose item revisions still match
  cannot keep the retired decoration. Bumping `history_rev` alone is not
  enough.
- The `mode.cycle` keybinding on Shift+Tab stays and toggles plan mode
  as defined here. The composer keeps intercepting Shift+Tab; it never
  reaches the browser.
- The existing `plan` companion kind is replaced by the [plan
  pane](#plan-pane) implementation; saved layout entries of the old kind
  migrate to it.
- The `docs/decisions.md` clause about draft moves and source-plan
  acceptance is removed with the implement flow.

## Plan record and states

Each plan is a durable record in the store, owned by `internal/workflow`
sequencing rules: thread, plan number, enabled, state, the artifact
revision the reviewer accepted, the artifact revision the user approved,
the proof list of the approved plan, the latest review result, the done
evidence. The files under the thread directory are what the agent
authors; the record is what transitions read and write. A file watcher
never authorizes a transition.

| Transition | Actor | How |
|---|---|---|
| off to planning | user | plan toggle; nothing is sent |
| planning to awaiting approval | agent | `plan_status ready` after the review pass accepts the artifact revision; the pane opens |
| awaiting approval to planning | agent or user | a comment round, or an artifact revision newer than the reviewed one |
| awaiting approval to implementing | user | Approve in the pane, bound to the reviewed revision; AO records it, the agent is told |
| implementing to done | agent | `plan_status done` with evidence, after the review pass accepts it |
| done to off | app | the block and hook payload are cleared |
| any to off | user | plan toggle; a running turn continues, the protocol is off, the plan is retained |
| off to the retained plan | user | plan toggle before done; the plan continues in its state |
| off to a new plan | user | plan toggle after done, see [Several plans](#several-plans-in-one-thread) |

Toggling on with an existing conversation starts planning on the user's
next message, never by itself. Phases inside implementing are the
agent's own bookkeeping in its notes and are not states. Approve and
done retries are idempotent on the revision they name; a stale reviewer
result for an older revision is discarded.

Plan mode needs the thread tools server on, since `plan_status` and
the reviewer ride it. Activation with the server off, or
with a provider that cannot install the compaction hook, reports the
missing capability and does not enable.

## The protocol

The block AO injects when plan mode turns on. Its exact text is an
implementation artifact; these are its requirements.

### Planning

- Investigate before asking. Facts about the repository, the tools and
  the environment are the agent's job. A question that a search or a run
  can answer is never asked.
- Intent is the user's. Repository evidence that conflicts with stated
  intent gets one plain question of the form "X exists and does Y; does
  that change anything?", never a history lesson.
- Every decision that is not clear-cut goes to the user, in rounds. A
  round holds the decisions whose prerequisites are settled, numbered,
  each with the options, a recommended answer and one line on why it is
  not clear-cut. Decisions whose answer depends on an open one wait for
  the next round. Planning is complete when no decision remains unasked
  and nothing is silently assumed.
- Trajectory before scope. The agent looks for what this work should
  clear rather than bridge, and for temporary steps it would be taking,
  and raises each as a decision. It never absorbs such work silently and
  never plans around it silently. The user decides.
- Answers are direct. A question gets the answer. Extra context appears
  only when the user needs it to decide. Research results stay in the
  agent's files unless the agent cannot tell what a finding means for
  intent.
- Architecture comes after intent. Technical design starts only when the
  intent decisions are closed.
- The plan carries its definition of done: every behavior the work
  introduces or touches and how each will be proven, derived from what
  the repository's guides say merge-ready means. A proof that needs something missing (fixture,
  harness, account, device) is built when the agent can build it within
  the plan's own work; if it cannot, the need is raised once, only when a
  person could supply it. A behavior nothing can prove is a decision for
  the user at planning: accept it unproven, or reduce scope. Done later
  cites that decision. Nothing is skipped silently. Validation tooling
  that future work will need is built properly, not as the quickest
  proof for this task. Before approval the agent makes no durable
  repository change; probes stay isolated.
- The agent writes `context.md` as it plans; it is the plan of record
  the reviewer reads. Small plans keep the artifact to markdown. The
  agent produces an HTML artifact when seeing the work whole helps the
  user judge it: layout, relationships, sequence, alternatives. Agent
  judgement, no threshold.

### Approval

The user approves in the plan pane; AO records the approval against the
reviewed revision and tells the agent. Comments sent with the approval
are dispatched in that same message and the agent starts implementation
after resolving them; no second send is needed. Comments on the artifact
after approval are plan changes: the agent applies them and continues,
and asks only when a comment touches a decision already made. A comment
that asks for detail is a question and gets an answer, never an applied
change.

### Implementation

- Phases. Build one piece, validate it to the plan's proofs, record the
  outcome in the notes, move on. The notes carry phase state so a
  compaction loses nothing.
- Stopping. Continue by default and get as much done as can be done
  cleanly. A decision that is easily reversible is taken, noted with
  the reason, and raised at the end. The agent stops and asks only when
  the work is blocked, or when a finding changes what was agreed or
  goes against what the user asked. The user's setup can narrow or
  widen this. "Don't stop" in chat overrides for the thread: the agent
  takes its best judgement on every finding, records what it chose and
  why, and brings it up at the end.
- Parked findings. Out-of-scope problems found while building go to the
  parked file: what was found, what it is, what should be done. They are
  not revisited until the task completes, and are reported at done. A
  cleanup that serves the task's own trajectory is raised as a decision
  unless the user's setup says to park such findings; a cleanup the plan
  depends on is never parked.
- Tests. Two kinds. Proof runs are throwaway, live in the thread
  directory or a temporary location, and are deleted after their result
  is recorded in the notes in one line each. Repository tests stay
  because they guard a behavior against future change. A repository test
  earns its place when it would fail if the behavior broke, sits at the
  lowest level that proves it fully, is not duplicated at another level
  and runs cheaply enough for its suite. Level follows where the
  behavior lives: user-visible flows at the browser or harness level,
  contracts between parts at integration, pure logic and edge cases at
  unit. Tests derive from the plan's proofs, so they assert intent and
  its edge cases, not the code's shape. The repository's guides name the
  suites and levels once.
- Durable context. At the end, refined context that future readers need
  goes into the repository where the project keeps such things, or by
  the agent's judgement of progressive disclosure when the project has
  no rule. Never a work log, never a chronology, never spread across
  guides that did not need it.
- Done. `plan_status done` carries the evidence: every proof from the
  approved plan's definition of done with a passing outcome or the
  planning decision that excused it, the parked findings, where durable
  context went, that proof runs are deleted. It is an explicit statement
  that the plan as agreed and this protocol were followed. A missing or
  failed proof is a user decision (reduce scope, accept, or fix), never
  done. `plan_status done` with incomplete evidence is refused and the
  state stays implementing.

### Review pass

A separate agent with the artifact, `context.md` and the user's
statements reviews before `plan_status ready` and before a done
declaration is accepted. It runs through the existing agent execution
and request settlement; no new scheduler. It reports wrong details,
conflicts with what the user said, decisions still missing and, at done,
proofs the evidence does not cover. It also checks the plan's technical
quality against the repository's own rules: ownership of each change,
whether something simpler or more sound exists, affected callers and
platforms, resource lifetimes, visible error handling, and whether the
proofs prove intent. Technical findings go back to the agent in
`context.md`; only decisions the user owns reach the user. Default on.
The agent may skip it only when the plan is one clear path with no
decision made on the user's behalf, no user-visible change and a blast
radius inside the touched files, and it states the skip in one line so
the user can veto. Large plans fan out one reviewer per piece. This pass
is part of the protocol, not a separate skill, and assumes nothing about
the user's own setup. If the reviewer cannot run (provider tools
disabled, capability missing), the plan is not marked ready and the
reason is stated; review is never claimed when it did not run.

## Files

Each thread owns a directory in AO's data directory beside attachments,
provider-agnostic, kept across restarts. Nothing here references a
provider's scratch location.

```
<data>/plans/<thread>/
  1-<slug>/
    plan.html | plan.md   the user's artifact
    context.md            the agent's plan of record, full detail
    parked.md             out-of-scope findings
    notes.md              phase progress, agent's words
    block.md              the injected protocol block and status, a projection of the record
```

- The agent writes these files with its ordinary tools under the
  thread's access tier. The plans root is an additional working
  directory for Claude (`--add-dir`, as attachments are) and a writable
  root of the Codex workspace-write sandbox, so tiers behave there as
  they do in the workspace: accept-edits writes without asking,
  approval-required asks, read-only cannot write and the agent says so.
  Nothing in the thread directory is committed.
- `context.md` is agent-owned in principle and readable by the user
  through the pane; it keeps stable section anchors so artifact
  references can jump to it.
- Proof runs go in the thread directory or a temporary location and are
  deleted before done.
- Lifetime. Deleting the thread, by the user or by the retention sweep
  (`Retention.Days`, default 30, zero disables), removes the directory.
  Archiving keeps it; unarchive finds it intact. A thread moved to
  another computer keeps its plans, carried as a section of the transfer
  bundle since provider transcripts cannot reconstruct them. A fork or a
  copy starts with no plan; plan mode there starts a new one.

### Several plans in one thread

Turning plan mode on after done starts a new numbered plan directory.
The agent receives the current plan path and a one-line index of earlier
plans (title, outcome, date). Earlier plans stay readable on disk and are
not injected. Earlier `parked.md` files are read at the start of a new
plan, since deferred work is what a next plan is most often about.
`context.md` and `notes.md` of finished plans are not needed: their
durable content already went into the repository.

## No settings

Plan mode adds organization and one set of instructions, nothing to
configure. What merge-ready means, what happens to out-of-scope
findings, what needs a human and where durable context goes come from
the user's own setup: the repository's agent guides, the user's global
instructions and the prompt. The protocol reads them and applies them.
When a repository says nothing about its definition of done, the agent
asks in the first planning round and writes the answer where the
repository keeps its guidance, as part of the durable-context step.

Defaults when the setup is silent are the protocol's own: every decision
that is not clear-cut reaches the user during planning; while building,
the agent continues unless blocked or unless a finding changes what was
agreed; nothing is parked silently. A setup that wants less ("park anything out of scope",
"technical detail is yours") says so, and "don't stop" in chat overrides
for the thread.

## Context injection

- Switch on: the next send carries the block once: protocol rules, the
  plan directory path, current status. AO writes the
  same block to `block.md`. Cached prefixes never change; nothing is
  injected into the system prompt.
- Compaction: AO installs one static `SessionStart` hook with the
  `compact` source on every session at spawn, through the session
  settings it already passes (Claude `--settings`, Codex thread config).
  `PostCompact` does not inject context on either provider. The hook
  runs a small AO-owned helper that prints `block.md` first and a
  bounded `notes.md` after it when the plan is active, and nothing
  otherwise. Its command never changes, so no session restart ever
  follows a state change; the files change instead. It composes with
  hooks a provider already installs (the Claude TUI provider's own
  `SessionStart` and compaction observers stay). A provider or version
  that cannot run it is a capability failure reported at activation.
- Status change: the next send carries the new status; AO rewrites
  `block.md` so the next compaction restores the current one.
- Done or off: `block.md` is removed and the hook injects nothing. No
  per-turn status line is needed: the block is in context until a
  compaction, and the compaction restores it.

## Plan pane

The `plan` companion kind, reimplemented, distinct from the browser
pane, which stays the agent's page. A second kind, `plan-context`, hosts
`context.md`, since companion identity is one pane per source and kind.

- Renders `plan.md` or `plan.html` and re-renders as the file changes,
  ignoring partial writes. Opens by itself when
  the plan becomes awaiting approval and stays until the user closes it
  or approves; approval closes it. The plan toolbar button and the
  thread header button open it at any time.
- Approve is enabled only while the shown revision is the reviewed one.
  A newer revision shows as unreviewed until the agent reports ready
  again. Plan mode always has an artifact; a task too small for one
  should not have been started in plan mode.
- HTML runs in an isolated origin with no reach into AO's transport,
  served through `internal/filepreview`, which supplies containment and
  origin isolation only. Above it: a viewer lifetime per open pane, a
  JSX entry convention (one `.jsx` or `.tsx` entry, relative imports
  under the plan directory), in-process transformation by esbuild's Go
  library when served, and React and ReactDOM provided by AO from disk
  with no network. Nothing loads until an HTML artifact opens and
  everything is released when the pane closes. A narrow postMessage
  bridge carries selection and anchor events to the pane and nothing
  else.
- Comments anchor to a DOM id in HTML or a line range in markdown, and
  always carry the artifact revision and the quoted text, so a removed
  or reused anchor still reads correctly. Every commentable block
  carries a stable id the agent assigns. Actions: send to the agent, ask
  for more detail. Comments batch into one reply on send and travel
  through the existing message delivery path. Sent comments collapse to
  a marker that expands on demand.
- A reference in the artifact to a `context.md` section opens
  `plan-context` at the section, where the user can read more or
  comment.
- A phase strip in the pane shows planning, awaiting approval,
  implementing and done, fed by the plan record; the expanded progress
  shows `notes.md` as written, with no parser and no second task system.
  The sidebar does not change.
- A selector switches between plans of the thread, defaulting to the
  current one; older plans open read-only.

## Composer progress

The composer shows the plan's overall progress beside the todos, in the
same expandable form: collapsed to an icon and the phase from the plan
record, expanded to `notes.md`. Todos are the agent's current work; this
is the run. It clears at done. Compact layout gets the same icon with the
expanded form fitted to the narrow width.

## Non-goals for this revision

- Dev-server pages and PDFs in the plan pane.
- An ephemeral side fork from a comment.
- Replacing the review pane's diff comments with the plan comment model.

## Sequencing

1. Spike the `SessionStart` compact hook on
   both providers: context arrives before the first post-compaction
   step, permissions unchanged, existing hooks preserved, automatic and
   manual compaction, resume, on/off/on.
2. Plan record, `plan_status`, file lifecycle, transfer
   section, crash recovery. Unexposed until it works end to end.
3. Pane, `plan-context`, JSX serving, comments, composer progress,
   review pass, hook. Still unexposed; the old flow untouched.
4. One cutover: toggle, provider flags, tool schemas, pane, send paths,
   the removal migration, table readers and writers, saved-layout
   migration, read-only plan cards kept.
5. Validate as an upgrade: mixed queues, drafts, imported plans, holders,
   forks, old transfer bundles, retained clients, and a fresh database.
   Go, frontend and the affected Playwright specs. Then the owning docs.

## Success criteria

- [ ] A full-access, auto, accept-edits, approval-required and read-only
      thread in plan mode prompts exactly as it does outside plan mode,
      on both providers, including writes to the plans root.
- [ ] No headless Claude session can enter the CLI's plan mode: the tools
      are absent and `set_permission_mode "plan"` is never sent.
- [ ] Old threads with `proposed_plan` items render the read-only card;
      no write path remains; the migration rewrites, restamps and drops
      as listed; a held window cannot retain retired decoration.
- [ ] The block arrives once on the switch-on send, never in the system
      prompt, and the compaction hook restores it and the notes before
      the first post-compaction step on both providers.
- [ ] Toggle off before done and on again continues the plan in its
      state; on after done starts a new plan with the index and parked
      carry-over.
- [ ] Approve binds to the reviewed revision, is refused for a newer
      unreviewed one, and is idempotent; duplicate approvals, restart
      mid-review, a stale reviewer result and comments mixed with
      composer text are covered at integration level.
- [ ] Comments, expand, jump to context and the plan selector work in
      the plan pane on desktop, compact and connected browsers; an anchor
      whose block was removed still renders with its quoted text.
- [ ] A JSX artifact renders with scripts in the isolated origin, cannot
      reach AO's transport or approve itself, and closing one viewer
      releases its resources without affecting another.
- [ ] `plan_status done` without complete passing evidence is refused
      and the status stays implementing.
- [ ] The thread directory is removed on thread deletion and by the
      retention sweep, survives archive and unarchive, moves with the
      thread, is absent on a fork or copy, and is never committed.
