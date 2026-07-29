# Workflows System — UI Specification (rev 2)

> Implementation-facing spec for the workflows surface, bound by
> `../workflows-system.md` **rev 2** and `../workflows-system-decisions.md`
> (D16–D23 + the rev-2 D11 amendment + **D32**, which removed every
> thread-spawning affordance). Rev 2 replaces rev 1's pane + sidebar
> section with a **full-surface overlay**; the queue surfaces (toggle, Queues
> section, drag priority, drain summaries) and the chat-enqueue confirm card
> are **deleted**, not restyled. Rev 1's run-state grammar, evidence blocks,
> sweep, and keyboard vocabulary survive and are restated here where they
> still bind. The `synthesis.html` mockup remains direction for row/receipt
> tone only — its pane frame is superseded.

Vocabulary (code/spec): **workflow** = definition, **run** = execution
(`work_items` row), **phase** = a step of a run, **unit** = one fan-out member
of a phase, **child run** = a call phase's invoked run. Surface copy stays
plain ("Runs", "History", "Needs you"); the surface is always called
**Workflows**, never "jobs".

---

## 1. Normative rules (every surface)

- **R1 — Two-hue attention.** Amber (`--warning`) ONLY for states a human must
  unblock (`needs-human` in all its typed reasons — gate, question, stuck,
  paused, interrupted, unit-failed, disposition — and their roll-ups). Red
  (`--error`) ONLY for `failed`. Everything else (running, done, cancelled,
  scheduled) is typographic — no colored dot, no badge.
  Done-awaiting-disposition counts neutrally, MUST NOT turn amber, and has no
  time-based escalation. A row shows at most one signal (dot + label); amber
  rows reuse the existing `status-glow-warning` ring + pulse conventions
  (`threadStatusPill.ts` / `app.css`).
- **R2 — No internals.** Variables, envelopes, JSON, schemas, gate traces
  never render on any human surface. Human evidence = narrative digest, diff,
  checks, cost. The word "variables" MUST NOT appear in UI copy — typed seeds
  render as plain form fields.
- **R3 — Only threads break out, and the surface never creates one.** The
  overlay owns all workflow navigation internally; a thread the run already has
  (phase, unit, the thread it is bound to) opens as a **normal thread pane**,
  never inside the overlay. Opening a thread **closes the overlay** (the pane
  tree underneath was never unmounted, so this is instant); reopening the
  overlay restores its stack. No affordance on this surface *starts* a
  conversation — every thread-spawning button was removed (D32).
- **R4 — Copy tone.** Terse, human, past-tense receipts ("Merged to main —
  fast-forward 3af92c1 · worktree cleaned"); metadata as `·`-separated
  fragments ("parked 7h", "2/5 · 14m · $1.84"). No exclamations, no emoji.

---

## 2. The overlay

### 2.1 Frame + lifecycle

- Rendered in `App.svelte` as a **sibling of `<PaneHost>`**, layered above it
  — never passed into PaneHost, never a `globalSurface`, never a pane kind.
  **The pane tree stays mounted and untouched while the overlay is open**
  (explicitly not the settings pattern); closing is a pure unmount of the
  overlay layer — zero pane rebuild, zero virtualizer resync.
- **Entry points**: the workflows footer button (§6), OS notifications (§9),
  and the command palette. All open the one overlay; deep links retarget its
  stack.
- **Dismissal**: `Esc` at home depth, clicking the scrim edge, the footer
  button again, or any thread break-out (R3). Closing never affects runs.
- **Persistence.** The stack (and sweep cursor) persists across overlay
  close/reopen within a session and across restarts via the existing app
  storage; restored entries whose target no longer exists drop from the top
  (home is always valid).
- Navigation state lives in `stores/workflowsOverlay.svelte.ts` (stack,
  project filter, sweep cursor). Data rides the typed `workflow:*` event
  channel via `stores/eventsWorkflow.ts`; RPCs through `stores/bindings.ts`.

### 2.2 Stack

Two levels: **home** › **run detail** (plus the terminal **all-clear** level
reachable only by finishing the sweep, §4.4). Rev 1's intermediate
workflow-detail level is folded into home — a workflow's runs and history
expand in place; there is no third navigation depth.

- **Header chrome.** Home: title "Workflows" + header controls (§3.1). Run
  detail: back chevron `‹` (tooltip "Back (esc)"), breadcrumb
  (`Workflows › <run title>`), state word.
- **Back semantics.** Back, `Esc`, and `Backspace` pop to home; at home,
  `Esc` closes the overlay. `Esc` first consumes transient state: armed
  confirm → open dialog → pop/close. Keys are suppressed while a text field
  has focus (typing guards in `keybindings.svelte.ts`).
- Level changes reset transient state (expanded diffs, armed confirms).

---

## 3. Home — projects, workflows, runs

Project-grouped, single column at every width. Groups ordered by project
name; a project renders only if it has workflow definitions, runs, or
automations.

### 3.1 Header controls

| Control | Behavior |
|---|---|
| Pause all | The §6 global kill switch: `❚❚` / `▶`. Tooltip "Pause stops new phase starts everywhere; in-flight turns finish". Toast on toggle. Not a queue — no ordering, no counts. |
| Project filter | All projects → each project. Filters groups and the sweep set. |
| `+ New run` | Opens intake (§7). |

`+ New run` is the only entry point. Authoring a workflow is work over the
definition files (`agent-overflow workflow new`, then an editor or a thread the
human opens themselves), not a button here (D32).

At narrow widths, controls past Pause-all collapse into `⋯`; row metadata
truncates end-first.

### 3.2 Project group anatomy

Within a group, in order:

1. **Needs attention** — parked runs (amber, oldest first), then failed
   (red). Row: signal dot + "Needs you"/"Failed" + title + right meta
   (`review gate · parked 7h`, `unit 3/5 failed · 2h`, `paused · yesterday`,
   `interrupted · app restarted`). Click → run detail (§4), landing inside
   the sweep.
2. **Running** — live runs: title + right meta `phase 3/5 · port-sections
   4 running · 2 waiting on provider:codex · 14m · $1.84`; the waiting-on
   fragment renders only when a phase/unit is blocked on capacity (§6 of the
   WHAT-spec). Live-activity line below in italic hint tone. Hover cancel
   `✕` arms to `stop this run?`; second click cancels; Esc disarms.
3. **Workflows** — the project's definitions (and shared ones, marked
   `shared`): name + chain summary (`plan → port ⇉ merge → validate ↺`).
   Read-only — editing a definition is file work, not a row action (D32).
   Automations render inline on their workflow row: `every 6h · next in
   3h 40m · 2 skipped` + enable/disable + **Run now**. A definition failing
   dry-run validation renders its first error inline, hint-toned.
4. **Recent** — collapsed by default (`▶ Recent · 12`): terminal runs, most
   recent first, receipt meta (`merged · 2d · $1.75`, `cancelled · worktree
   kept`, `discarded — branch dropped, record kept`). Rows open historical
   run detail (§4.6).

**Continuity notes** (jobs that declare them): one collapsed line on the
workflow row, expanding to the editable markdown block — unchanged from
rev 1.

---

## 4. Run detail

The resolution surface. Everything from rev 1's run detail survives except
queue rows; the phase list becomes a **tree**.

### 4.1 Header block

- Row 1: project chip · **state word** (amber "Review gate" / "Question" /
  "Paused" / "Interrupted" / "Unit failed"; red "Failed"; neutral "Done" /
  "Running" / "Cancelled") · sweep counter when parked (§4.4).
- Row 2: run title. Row 3 (hint): `workflow · phase 4/5 · parked 7h · $3.10`
  — cost is the **root-tree total** (children included); automation runs add
  `spawned by jira-poll · every 6h`; a bound run adds `→ <thread title>`
  (click opens the thread, R3).

### 4.2 The run tree

Ordered phases; two node kinds expand:

- **Fan-out phase** — expands to its units: `✓ port-auth · 12m · $0.84` /
  `● port-catalog · <live activity>` / `○ port-search · waiting on
  provider:codex` / red `✗ port-vuln ×2`. Every unit row with a thread is
  openable (R3). The join renders as the phase's final unit row.
- **Call phase** — expands to the **child run** inline (its own phase rows,
  recursively). The child's header row shows its workflow name + state +
  cost; a `↳ depth 2` fragment appears past depth 1. Child runs have no
  bind/notify affordances (D18) — resolution actions on a parked child
  render inside the parent's tree.

Retried phases render one row per attempt (`check · attempt 2`); historical
attempts stay openable (§4.6).

### 4.3 Per-state digest, evidence, actions

The two-row digest (`WHAT HAPPENED` / `WHAT IT NEEDS`) opens every state.
Action row is a fixed footer, primary first; keys per §8.

| State | Evidence | Action row |
|---|---|---|
| **gate** | Checks row; diff file list expanding hunks in place; full review → the real ReviewPane (§4.7). | `Approve → <next>` (a, primary) · `Request changes` (r — inline optional note, Enter commits; rides as loop feedback) |
| **question** | Question quote block; suggested answers as digit-hinted buttons; answer input ("Answer — the phase resumes where it yielded") + Send. | None — the answer input IS the affordance. Digits pick + send; `a` focuses input; Enter sends. |
| **failed** (`check-failed-genuine`, `child-failed`) | Failing check line; latest diagnosis quote. | `Rerun with guidance` (a, primary — new attempt seeded with the diagnosis as feedback) · `Discard` (r, danger, §4.5) |
| **blocked** (every other `needs-human` reason: stuck, agent-error, wiring-error, setup-failed, budget-exhausted, stalled, retries-exhausted) | Same evidence as **failed** — a run that could not finish asks the same question whichever state it stopped in. | `Resume` (a, primary — re-enters the phase with a fresh attempt, after the human clears whatever blocked it) · `Discard` (r, danger, §4.5) |
| **paused / interrupted** | Receipt line (`paused by you · yesterday` / `interrupted — the app was restarted`); partial-envelope digest if one was captured. | `Resume` (a, primary — next attempt, same provider thread, continue message) · `Discard` (r, danger, §4.5) |
| **unit-failed** | The failed unit's row highlighted in the tree; its failing check/diagnosis inline; survivors' states visible above. | `Retry unit` (a, primary) · `Retry all failed units` (u — repairs every failed unit of the attempt in one action, D33) · `Drop unit — join proceeds without it` (recorded in the gate trace) · `Take over unit` (t — detaches the unit and opens the thread it is ALREADY running in) · `Discard` (r, danger, §4.5) |
| **taken-over** | The steered phase thread's state; the run is under human control. | `Finish takeover` (a, primary — one finalize turn re-attaches the schema) · `Discard` (r, danger, §4.5) |
| **done** | Checks row; disposition block (manual: merge / PR / discard; auto-merge projects show the receipt + policy + undo line). After Create PR: the PR block with `Review comments (N)` + `Discuss this PR` riding the linked thread (§4.7). Outputs block (§4.8). | Manual: `Merge to main` (a, primary) · `Create PR` · `Discard` (r, danger, §4.5). Any run adds `Bind to thread…` in the `⋯` menu — it binds an EXISTING thread and never creates one. |
| **running** | The run tree, live. | `Pause` (interrupt in-flight turns → park paused) · `Open phase thread` · `Stop this run` (danger, teardown → cancelled) |
| **cancelled** | Receipt `cancelled · worktree kept`. | `Discard` (danger, §4.5) · `Back` |
| **resolved (this session)** | Digest + green receipt ("Approved — routing to docs", "Resumed — the phase continues its session", "Unit dropped — join proceeds over 4 of 5"). | `Back` (esc) |

Merge MUST refuse on conflict/dirty base and park `needs-human(disposition)`
— never forced, never silent.

**Taking a run over is a send, not a button (D32).** Open the phase thread from
the tree and type: the send path interrupts the in-flight turn, detaches the
attempt from engine control, and parks the run `needs-human(taken-over)`, from
where `Finish takeover` hands it back. No row here spawns a thread to take a
run over in — the thread the phase is already running in is the one to steer.

### 4.4 Needs-attention sweep (j/k)

Unchanged from rev 1 except the set: all parked runs — every `needs-human`
reason including paused/interrupted/unit-failed — **plus
done-awaiting-disposition**, app-wide (respecting the project filter),
oldest-parked first, wrapping. `j`/`k` step; acting shows the receipt then
auto-advances (~650ms); exhaustion pushes **all-clear** (centered ✓,
"Nothing needs you", session summary, `Back` → home). Child-run parks appear
as their **root** run (one sweep stop per tree); the tree opens at the
parked node.

### 4.5 Discard — preview is consent (D23)

Discard on any resting run opens a **loss preview** dialog before anything
is destroyed: one row per worktree in the run's tree (primary,
sub-worktrees, child worktrees) — `branch · N dirty files · M unmerged
commits` — and the artifact note ("artifacts already captured survive").
Confirm tears the tree down through the teardown contract and drops
run-created branches whose commits never landed; the run record is kept.
Toast: "Discarded — 3 worktrees removed, record kept". There is no
un-previewed destructive path anywhere on the surface.

### 4.6 Historical runs and threads

Any terminal run opens in its historical state; every phase/unit/child
attempt's thread (completed, failed, superseded) opens from the tree
(`work_item_phases.thread_id`). History rows on home (§3.2) push the same
view.

### 4.7 PR follow-ups + full review

Unchanged from rev 1: `Review comments (N)` (lazy count, single-flight,
untrusted quoting) and `Discuss this PR` ride the run's one linked thread;
"open full review" opens the **real ReviewPane** as a normal pane (overlay
closes, R3). No parallel diff renderer.

### 4.8 Outputs / deliverables

Runs whose workflow declares outputs render an **Outputs** block above the
action row on terminal states: named values as kv rows; artifact files open
on click — markdown/HTML in the app preview, else the system opener. A call
phase's outputs render on the child; the root's declared outputs are the
run's deliverables. Artifacts survive worktree discard.

---

## 5. Intake

### 5.1 New-run dialog

Modal-primitive dialog, unchanged fields except the footer: Project · Goal ·
Workflow picker (invalid definitions greyed with first error) · Base branch ·
typed seed inputs as plain fields · step-mode checkbox. Footer: primary
**`Start`** (the run starts immediately — no position, no prediction) ·
`Cancel`. Toast: "Started — <workflow> on <project>".

### 5.2 From chat — the `/workflow` command

Rev 1's proposal confirm card (§7.2) and its MCP plumbing are **removed**.
Agents start runs through the `agent-overflow` CLI under normal bash approval
(D17, D30). The only run-shaped UI in a chat timeline is the **wake message** a
bound run delivers, which is an ordinary user-role message — no custom card.

**`/workflow` is a composer slash command (D31), not a palette action.** Typing
`/` as the first character of a draft opens a completion menu of registered
commands — the same popover family as the `@`-mention completion, filtering as
the user types, arrow / Enter / Tab / click to select, Escape to dismiss.
Selecting one completes the **word** and nothing else: the draft then reads
`/workflow ` plus whatever the user goes on to type. There is no token, no
chip, and no inserted block.

While the draft's leading word matches a registered command it renders in the
accent colour, in the composer and in the sent message in chat history. That is
the whole visual vocabulary.

**Expansion happens at send time, on the backend.** When a sent message's first
word names a registered command, the send path appends the command's block to
the payload the PROVIDER receives — typed text first (it is the instruction),
block second (it is context) — and stores the user row with exactly what was
typed plus a meta marker naming the command that expanded. History colours the
word from that marker, never from a live registry match, so a row stays
truthful about what actually ran. The session's system prompt is never touched:
a per-thread system prompt would invalidate provider prompt caching for every
turn to serve one message.

---

## 6. Sidebar integration

**One footer button, nothing else.** `WorkflowsFooter.svelte` directly above
`SettingsFooter.svelte`: icon + "Workflows" + the single global
needs-attention count (amber, only when > 0). Click toggles the overlay.
Rev 1's per-project sidebar section (`WorkflowsSection.svelte`) is
**deleted** — runs are not sidebar citizens.

**Thread-list exclusion stands**: phase/unit (`workflow`), studio
(`workflow-studio`), and triage (`workflow-triage`) threads carry their
`threads.mode` values and MUST NEVER appear in normal thread lists, search,
or pickers — excluded by mode in `utils/sidebarTree.ts` /
`ProjectThreadList.svelte` / `UnifiedThreadPicker.svelte`, never by title.
The exclusion covers three modes but only two are still minted: D32 removed
the studio spawner, and `workflow-triage` is now written only by the PR
follow-up surfaces (§4.7), which reuse the run's one linked thread. Existing
rows in either mode stay hidden — the mode plumbing is data compatibility, not
a live affordance.
Once opened (from the overlay or a wake message reference) they behave as
completely normal thread panes. **Exception by design**: a thread a run is
*bound to* (§4.1) is a normal user thread that was never mode-excluded —
binding never hides a thread.

---

## 7. OS notifications + deep links

- A run entering **`needs-human`** (any typed reason) or **`failed`** fires
  one OS notification — title = run title, body = the one-line "what it
  needs". Click → app foreground, overlay at that run's detail, inside the
  sweep. Thread-bound runs also wake their thread (D17); the notification
  still fires and the badge stays authoritative.
- `done` / `running` never notify. Rev 1's coalesced drain summary is
  **deleted** with the queue.
- Child runs never notify (D18) — the root run carries the tree's attention.

---

## 8. Keyboard bindings

Registered through the existing keybinding/command registries, scoped to the
open overlay; suppressed while a text field has focus.

| Key | Context | Action |
|---|---|---|
| `Esc` | run detail | disarm confirm → close dialog → back to home |
| `Esc` | home | close the overlay |
| `Backspace` | run detail | back to home |
| `j`/`→`/`↓` · `k`/`←`/`↑` | run detail, parked set | next / previous needs-attention run (wraps) |
| `a` | gate / question / failed / blocked / paused / unit-failed / taken-over / done | primary action (approve / focus answer / rerun / resume / retry unit / finish takeover / merge) |
| `r` | gate / failed / blocked / paused / unit-failed / taken-over / done / cancelled | request changes / discard (opens the §4.5 preview) |
| `t` | unit-failed | take over the failed unit — the only `t` binding left (D32) |
| `u` | unit-failed | retry every failed unit of the attempt at once (D33) — the usage-limit recovery, where pressing `a` once per unit is the same repair typed N times |
| `1`–`9` | question | pick + send the nth suggested answer |
| `Enter` | gate / question input | toggle first diff file / send answer |

---

## 9. Empty + quiet states

- Nothing defined anywhere → single empty state: one short line saying what a
  workflow is, and no button (D32 removed the only one it had). A project with
  definitions but no runs → its Workflows list only. No parked runs → footer
  badge shows no count; sweep entered empty → all-clear directly.
- The normal-view test stands: **workflows are invisible until they have a
  reason not to be** — a quiet system is one footer row.

---

## 10. Remote posture

Remote browsers get **view-only** workflows: the overlay renders fully;
every mutating affordance (pause-all, intake, action rows, notes, Run now,
enable/disable, discard, bind) is disabled with tooltip "Local only".
Mutation RPCs classify `LocalOnlyMethods` in
`internal/transport/internalmethods.go`. Remote gate-approval is explicitly
out.

---

## 11. Non-goals (normative)

- NO variables / envelope / JSON / schema / gate-trace rendering on any
  human surface — those exist for agents through the `agent-overflow` CLI.
- NO affordance that spawns a chat thread (D32): no "Continue with agent",
  no "Open in thread" seed+bind, no triage-agent shell, no studio thread.
  Opening a thread the run ALREADY has (phase, unit, bound) stays.
- NO queue surfaces: no toggle, no Queues section, no drag priority, no
  position predictions, no drain summaries.
- NO workflows pane, NO per-project sidebar section, NO pane-kind or
  pane-persistence integration — the overlay is the only surface and panes
  never unmount beneath it.
- NO chat proposal cards — agents use the CLI; the wake message is a plain
  message.
- NO kanban / global work board; NO inspection modal; NO workflow-settings
  forms beyond enable/disable + Run now + notes (everything richer is studio
  work over files).
- NO un-previewed destructive action (§4.5).
- NO remote mutation of any kind.

---

## 12. Integration point map

As shipped. Paths under `frontend/src/lib/` unless noted.

| Concern | Where |
|---|---|
| Overlay frame (§2.1) | `components/workflows/WorkflowsOverlay.svelte`, mounted in `App.svelte` through `primitives/LazyOverlay.svelte` as a sibling of `<PaneHost>` (never a `globalSurface`, never a pane kind) |
| Navigation (§2.2) | `stores/workflowsOverlay.svelte.ts` — stack, project filter, sweep cursor, armed confirm, dialog; restart persistence via `stores/appStorage.ts` |
| Data cache | `stores/workflowRuns.svelte.ts` (runs, catalogs, automations, costs, per-run detail with eviction, session receipts) + the pure projections in `stores/workflowData.ts` |
| Events / RPC | `stores/eventsWorkflow.ts` via `events.ts`; `stores/bindings.ts`; typed `workflow:*` channel |
| Home (§3) | `WorkflowsHome.svelte`, `WorkflowsHomeControls.svelte`, `WorkflowProjectGroup.svelte`, `WorkflowRunRow.svelte`, `WorkflowDefinitionRow.svelte` |
| Run detail (§4.1–§4.2) | `WorkflowRunDetail.svelte` (coordinator), `WorkflowRunHeader.svelte`, `WorkflowRunTree.svelte`, and the pure tree assembly in `utils/workflowRunTree.ts` |
| Evidence (§4.3) | `WorkflowEvidence.svelte` dispatching to `WorkflowGateDiff.svelte` / `WorkflowDiff.svelte` / `WorkflowFailureEvidence.svelte` / `WorkflowDisposition.svelte` / `WorkflowOutputs.svelte` / `WorkflowJobNotes.svelte`; envelope reads in `utils/workflowEnvelope.ts` |
| Action row (§4.3) | `WorkflowActionRow.svelte` over the pure table in `utils/workflowActionRows.ts`; dispatch in `stores/workflowActions.ts`; receipts/toasts/auto-advance in `stores/workflowResolve.ts` |
| Sweep (§4.4) | `stores/workflowSweep.ts` over `workflowData`'s sweep helpers; `WorkflowAllClear.svelte` |
| Discard (§4.5) | `WorkflowDiscardDialog.svelte` — loss preview is consent, and it resolves through the same `stores/workflowResolve.ts` |
| Intake (§5.1) | `WorkflowIntakeDialog.svelte` + `WorkflowSeedFields.svelte` over `utils/workflowIntake.ts`, on the `components/primitives/Modal.svelte` shell |
| Footer button (§6) | `components/sidebar/WorkflowsFooter.svelte` in `Sidebar.svelte`, above `SettingsFooter.svelte` |
| Deep links (§7) | `stores/notificationActivationQueue.ts` → `openWorkflowRunInOverlay` |
| Keyboard (§8) | commands in `stores/workflowCommands.svelte.ts`, flags in `stores/builtinCommands.svelte.ts`, chords in `internal/keybindings/keybindings.go` |
| Signals (R1) | `utils/workflowRunSignal.ts` — the one place a state maps to a hue |
| Thread opens (R3) | `stores/workflowThreads.ts` — opens existing threads only, never creates one; thread exclusion by mode in `utils/sidebarTree.ts`, `ProjectThreadList.svelte`, `UnifiedThreadPicker.svelte` |
| PR review | `stores/reviewPane.svelte.ts` companion flow (overlay closes; ReviewPane opens as a pane) |
| `/workflow` command (§5.2) | registry + trigger rules in `components/composer/slashCommands.ts`, menu state in `composerSlash.svelte.ts`, popover `ComposerSlashPopover.svelte`, accent word `ComposerCommandHighlight.svelte` (composer) and `chat/UserMessage.svelte` (history, from `utils/userMessageMeta.ts`); send-time expansion in `app_composer_commands.go` over the block `app_workflow_composer.go` resolves |
| Toasts | `stores/toast.svelte.ts` |
| e2e | `e2e/tests/workflows-overlay.spec.ts` |

Drift from the pre-build plan, deliberately: the reactive cache is
`stores/workflowRuns.svelte.ts` (rev 1 had no equivalent — `workflowData.ts`
stays pure so it is unit-testable without a Svelte runtime), and the run
detail is a coordinator over per-section components rather than one file.

**Deleted with rev 2** (remove, don't orphan): the `'workflows'`
`PaneLayoutKind` + PaneHost mount branch + pane persistence entries,
`stores/workflowsPane.svelte.ts`, `stores/workflowsSidebar.svelte.ts`,
`WorkflowsSection.svelte` + sidebar wiring, `WorkflowsPane.svelte` and the
pane-stack chrome (`WorkflowOverview*`, `WorkflowQueue*`, breadcrumbs), the
intake queue-position footer, the proposal-card component + its
`workflow_proposal` item rendering, deep-link pane machinery, and their
tests/e2e specs (replaced by overlay equivalents).
