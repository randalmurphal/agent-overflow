# Workflows System — UI Specification

> Implementation-facing spec for the M3 surfaces, extracted from the approved mockup
> (`synthesis.html`) and bound by `../workflows-system-decisions.md` (D3, D4, D11 +
> amendments, D12, D13, D14, D15). Where mockup and log disagree, **the log wins** —
> divergences applied here are listed in §14. The mockup is direction, not chrome:
> implement with native components, tokens, and theme (D11 amendment 3).

Vocabulary (code/spec, per D11 amendment 2): **workflow** = definition, **run** =
execution (`work_items` row), **phase** = a step of a run. Surface copy stays plain
("Runs", "History", "Needs you"); the surface is always called **Workflows**, never
"jobs" (D11 amendment 1).

---

## 1. Normative rules (every surface)

- **R1 — Two-hue attention.** Amber (`--warning`) ONLY for states a human must
  unblock (`needs-human` gate/question and their roll-ups). Red (`--error`) ONLY for
  `failed`. Everything else (running, queued, done, cancelled, scheduled) is
  typographic — no colored dot, no badge. Done-awaiting-disposition counts neutrally,
  MUST NOT turn amber, and has no time-based escalation (D11 amendment 1). A row
  shows at most one signal (dot + label); amber rows reuse the existing
  `status-glow-warning` ring + pulse conventions (`threadStatusPill.ts` / `app.css`).
- **R2 — No internals.** Variables, envelopes, JSON, schemas, gate traces never
  render on any human surface (D11). Human evidence = narrative digest, diff, checks,
  cost. The word "variables" MUST NOT appear in UI copy — typed seeds render as plain
  form fields (D11 amendment 1).
- **R3 — Only threads break out.** The workflows surface is one pane with internal
  stacked navigation; any thread (phase, triage, studio) opens as a normal thread
  pane beside it, never inside it (D11 amendment 2).
- **R4 — Copy tone.** Terse, human, past-tense receipts ("Merged to main —
  fast-forward 3af92c1 · worktree cleaned"); metadata as `·`-separated fragments
  ("parked 7h", "2/5 · 14m · $1.84"). No exclamations, no emoji.

---

## 2. The workflows pane

### 2.1 Pane kind + lifecycle

- New `PaneLayoutKind` `'workflows'` in `stores/paneLayout.svelte.ts` — a first-class
  pane (NOT a companion, no `sourcePaneId`), mounted by a new branch in
  `PaneHost.svelte`, normal width/divider/density behavior.
- **Singleton.** At most one workflows pane in the layout. Every entry point (sidebar
  rows, footer badge, notifications, deep links) MUST reuse it — retarget its stack,
  `revealPane`, focus — never open a second. If none exists, append at the right end
  of the pane row and focus.
- Closes like any pane (`×`, `Ctrl/Cmd+W`). Closing never affects runs — the engine
  is the source of truth.
- **Persistence.** Pane + navigation stack persist via `paneLayoutPersistence.ts`. On
  restore, stack entries whose target no longer exists are dropped from the top
  (overview is always valid).
- Navigation state lives in a new `stores/workflowsPane.svelte.ts` (stack, filter,
  sweep cursor). Data rides a new typed `workflow:*` event channel (D7) via a new
  `eventsWorkflow.ts` fanned out from `events.ts`; RPCs through `stores/bindings.ts`.

### 2.2 Stacked navigation

Stack: `overview` › `workflow detail` › `run detail` (plus a terminal `all-clear`
level reachable only by finishing the sweep, §5.4).

- **Header chrome.** Depth 0: pane title "Workflows" + overview controls (§3.1).
  Depth > 0: back chevron `‹` (tooltip "Back (esc)"), clickable breadcrumbs for each
  ancestor (`Workflows › build-and-validate`), `›` separators, current level's label
  as pane title. Crumb click pops to that level.
- **Back semantics.** Back button, `Esc`, and `Backspace` pop one level; at depth 0
  they do nothing (MUST NOT close the pane). `Esc` first consumes transient state:
  armed cancel confirm → open dialog → pop. Keys apply only while the pane is focused
  and never while a text field has focus (typing guards in `keybindings.svelte.ts`).
- **Transitions.** Pushed levels slide in (~200ms ease-out, translateX + fade); honor
  `prefers-reduced-motion`. Level changes reset transient state (expanded diff files,
  armed confirms, PR sub-views).
- **Deep-link targets** (all reuse-or-create per §2.1, seeding the full stack so
  back/crumbs work): sidebar run row → run detail (`overview › workflow › run`);
  footer badge and sidebar section header → overview; amber "N need you" on a
  workflow row → sweep (§5.4) at that workflow's oldest parked run; per-item OS
  notification → that run's detail (§10); drain-summary OS notification → the triage
  agent thread pane (§10), not the workflows pane.

---

## 3. Level 1 — Overview

Workflow-centric (D11 amendment 1): lists **workflows** with aggregate state; runs
are subordinate. Single-column list at every width.

### 3.1 Header controls (depth 0 only)

| Control | Behavior |
|---|---|
| Queue toggle | `❚❚ Active` / `▶ Paused`; tooltip "Pause stops new starts; running items finish" (D3.1). Toast on toggle: "Queue active — draining by priority" / "Paused — running items finish; nothing new starts". |
| Slots | Dots + `n/m` concurrency slots in use (tooltip "2 of 3 concurrency slots in use"). Read-only. |
| Project filter | All projects → each project. Filters workflow rows (shared-scope always visible) and Up-next rows. |
| `+ New run` | Opens intake (§7). |
| `+ New workflow` | Opens a **studio thread** as a normal thread pane (D4.1 gap 1). No editor surface. |
| Triage | Opens/resumes the **triage agent** thread as a normal thread pane (D4.9, D4.1 gap 3). |

At narrow widths, controls past the queue toggle collapse into a `⋯` overflow menu;
row metadata truncates end-first. No other layout change — the single-column list
satisfies the ratified narrow-width flag by construction.

### 3.2 Sections + workflow row anatomy

Sections in order, uppercase headers with counts: **Active** (any live run),
**Scheduled** (automation-driven, D3.2), **Idle**. A workflow appears in exactly one.

Row (card-bordered, whole row clickable → workflow detail):

- Line 1: workflow name + right-aligned aggregate: amber `N need you` (bold,
  clickable → sweep §5.4) prefixed when N > 0, then neutral fragments
  `2 running · 1 queued · 1 to dispose` or `idle`. Scheduled: `next run today 18:02`
  (+ `· N queued`). Idle: `idle · last run 3d ago`.
- Line 2 (hint-toned): `scope · meta · chain`, e.g. `shared · 5 phases · 1 human gate
  · plan → implement → check → review → docs`; automations: `project · automation ·
  every 6h · in 3h 40m`.

### 3.3 Queues section (per-project queues; M4 rulings amendment)

Below the sections when ≥1 project has queued **or running** runs; hidden when
empty. Renders as a **list of per-project queues** ("Queues · N", one group per
project, ordered by project name) per the M4 ruling — queues are per project,
each startable/stoppable with its own concurrency limit.

- **Group header**: project color dot + name, `running/effective-cap` slots
  (effective cap = min(project cap or global, global concurrency)),
  `Pause`/`Resume` toggle, and a concurrency select (`Global` = inherit, 1–32).
  A running-only project still renders — the header is its control surface.
- Row (within a group): hover-revealed drag grip, `#position`, title, right meta
  `workflow · queued 3h` / `spawned 6m ago` (automation), `· held` while the
  global queue **or the project** is paused. Click opens the run's detail.
- **Drag-reorder** sets priority within the project (`sort_position`, D8): drop
  indicator above/below, persists immediately, toast "Priority reordered — the
  drain picks it up immediately". No cross-project drag.

### 3.4 Overview states

| State | Rendering |
|---|---|
| No workflows defined | Empty state: short line + `+ New workflow` (studio). |
| All idle, queue empty | Sections render; Queues hidden; no amber anywhere. |
| Attention pending | Amber count on the owning workflow row only (R1). |
| Queue paused | Toggle `▶ Paused`; queued rows append `held`. |
| Remote | Identical rendering; mutating controls disabled (§12). |

---

## 4. Level 2 — Workflow detail

Sub-line under the header: `scope · meta · chain` (§3.2 line 2). Header affordance
**Edit** opens/resumes the workflow's studio thread as a normal thread pane (D4.1
amendment).

### 4.1 Content, top to bottom

1. **Next-run banner** (automations only): `◷ Next run today 18:02 · in 3h 40m` +
   **enable/disable** toggle + **Run now** button (D4.1 gap 2). Run now enqueues
   through the normal queue. Anything richer (cron, seeds, conditions) is studio
   work — no forms.
2. **Runs** — every live run of this workflow (§4.2).
3. **History** — terminal runs, most recent first: title + receipt meta (`merged ·
   2d · $1.75`, `cancelled · yesterday · worktree kept`, automation spawns `ran 12:02
   · spawned the queued run · $0.14`). History rows are receipts over the persisted
   run record and are **openable**: click pushes that run's detail in its historical
   terminal state, from which every phase attempt's thread opens (§5.6, D11
   amendment 3).
4. **Continuity notes** (D12) — only when the workflow declares them; one collapsed
   line `▶ Continuity notes — carried across runs · rewritten after last run`,
   expanding to an editable markdown block (saved on blur/debounce), hint "editable —
   the next run reads this; a terminal phase may rewrite it". Notes live on the
   workflow, never per run.

### 4.2 Run row anatomy + states

Row: project dot · [signal] · title · right meta · [hover affordance]. One signal max
(R1). Click → run detail.

| Run state | Signal | Meta (right) | Extra |
|---|---|---|---|
| gate | amber pulse dot + "Needs you" | `review gate · +34 −12 · checks ✓ · parked 7h` | glow ring |
| question | amber pulse dot + "Needs you" | `asks — <short question> · parked 9h` | glow ring |
| failed | red dot + "Failed" | `check ×3 · genuine · 11h` | |
| running | none | `implement · 2/5 · 14m · $1.84` | live-activity line below in italic hint tone (D4.6); hover cancel `✕` arms to `stop this run?`, second click cancels, Esc disarms |
| queued | none (dim) | `queued · #2` (+ `· spawned 6m ago`) | |
| done | none (dim) | `done · to dispose · $0.92 · 2h` | |
| cancelled | none (dim) | `cancelled · worktree kept` | |

Cancel performs engine teardown (D9); toast "Teardown — turn stopped, locks released,
worktree kept". Empty list renders `no live runs` in hint tone.

---

## 5. Level 3 — Run detail

The resolution surface; absorbs the stepper (D11 amendment 2).

### 5.1 Header block

- Row 1: project chip (dot + name) · **state word** (amber "Review gate" /
  "Question"; red "Failed"; neutral "Done" / "Queued" / "Running" / "Cancelled") ·
  sweep counter when parked (§5.4): `2 of 4` + progress dots (current highlighted,
  resolved green) + `j` `k` hints.
- Row 2: run title. Row 3 (hint): `workflow · phase 4/5 · parked 7h · $3.10`;
  automation runs add `spawned by jira-poll · every 6h`; done shows `finished 2h ago`.

### 5.2 Digest

Every state opens with the two-row generated digest (D4.3): `WHAT HAPPENED` / `WHAT
IT NEEDS` label-value pairs, the ask stated plainly ("A human eye on the store-shape
change before docs runs — the review gate always parks for you." / "Nothing.").

### 5.3 Per-state evidence + action row

Action row is a fixed footer, primary first; keys `a` (advance/approve), `r`
(reject/discard), `t` (thread — take over / hand off) are constant across states
(§9).

| State | Evidence block | Action row |
|---|---|---|
| **gate** | Checks row (`✓ build 9s ✓ test 21s …`); diff file list, each row `path +a −d` expanding hunks in place; leads with the diff (D4.4). Full review → the real ReviewPane (§5.7). | `Approve → <next-phase>` (a, primary) · `Request changes` (r — reveals an inline optional-note input, Enter commits; note rides as loop feedback per D5) · `Take over` (t) |
| **question** | Question as a quote block; suggested answers as buttons with digit hints lifted from the question; answer input (placeholder "Answer — the phase resumes where it yielded") + Send. Answering resumes the yielded turn — no restart (WHAT-spec §7). | `Take over instead` (t). Digits pick + send a suggestion; `a` focuses the input; Enter sends. |
| **failed** | Failing check line `✗ go test ./internal/triage — TestParallelDispatch ×3 · genuine`; latest diagnosis as italic quote (`diagnosis #3: "…"`). | `Continue with agent` (t, primary — §8.2) · `Re-enqueue with guidance` (a — requeues with the diagnosis as feedback) · `Discard` (r, danger) |
| **done** | Checks row; disposition (D3.3): manual view offers merge/PR/discard. After Create PR: PR block (`⎇ PR #214 · branch · open ↗`, `Review comments (N)` → §5.7, `Send comments to the agent` → run returns to Running with a fix turn, D11). Auto-merge projects show a **receipt** instead: green `✓ Merged automatically · today 06:12` + kv rows merge (`branch → main · fast-forward · sha`), policy ("project opted in; a conflict or dirty base parks for you instead"), undo (`git revert <sha>`). | Manual: `Merge to main` (a, primary) · `Create PR` · `Continue with agent ↗` (t) · `Discard` (r, danger). Auto-merge/PR views: `Continue with agent ↗` only. |
| **running** | Phase list: `✓ name · duration` (done), `● name · <live activity>` (running), `○ name` (waiting). Retried phases render one row per attempt (`check · attempt 2`); every attempt row with a thread is openable (§5.6). | `Open phase thread` · `Stop this run` (danger, teardown) |
| **queued** | Digest + queue position. | `Remove from queue` (danger); toast notes automation runs get re-proposed next cycle. |
| **cancelled** | Receipt `cancelled · worktree kept`. | `Discard worktree` (danger, guarded — §5.8) · `Back` |
| **resolved (this session)** | Digest + green receipt line ("Approved — routing to docs", "Answered — '<answer>' · the phase resumes its turn", "Re-enqueued with the diagnosis as guidance — position 3", "Merged to main — fast-forward 3af92c1 · worktree cleaned", "Discarded — branch dropped, record kept"). | `Back` (esc) |

Merge MUST refuse on conflict/dirty base and park `needs-human(disposition)` — never
forced, never silent (D3.3).

### 5.4 Needs-attention sweep (j/k)

- **Sweep set:** all parked runs — gate, question, failed, **plus
  done-awaiting-disposition** (D11 amendment 1) — app-wide (respecting the overview
  project filter), oldest-parked first, wrapping.
- While run detail shows a member, `j`/`→`/`↓` steps next, `k`/`←`/`↑` previous; each
  step retargets the full stack (crumbs follow the new run's workflow). Resolved runs
  stay in the cycle this session (receipt renders) but are skipped on auto-advance.
- Acting shows the receipt in place, then (~650ms) auto-advances to the next
  unresolved member. When none remain, the pane pushes **all-clear**: centered green
  ✓, "Nothing needs you", summary (`4 resolved — 1 approved · 1 answered · 1 handed
  off · 1 merged · $11.54 reviewed`), `Back to workflows` (esc → overview).
- Entry points: amber counts (workflow row, sidebar roll-up) and per-item
  notifications land inside the sweep with the counter visible.

### 5.5 Outputs / deliverables (D13)

Runs whose workflow declares outputs render an **Outputs** block above the action row
on terminal states: named values as kv rows; artifact files as rows opening on click
— markdown/HTML in the app's preview surface, everything else via the system opener.
Artifacts come from the per-run artifact store and stay openable after worktree
discard (D14).

### 5.6 Historical phase threads (D11 amendment 3)

Run detail for ANY run — live or historical — exposes the thread of every phase
attempt (completed, failed, superseded retries) via the phase list; clicking opens it
as a normal thread pane. Thread ids come from the denormalized
`work_item_phases.thread_id`.

### 5.7 PR review integration

`Review comments (N)` and any "open full review" affordance MUST open the **real
ReviewPane** as its own pane — the existing review companion (`reviewPane.svelte.ts`
→ `openCompanion(paneId, 'review')`) with the workflows pane as `sourcePaneId`,
targeted at the run's worktree diff or PR. No parallel diff renderer; the inline gate
file list (§5.3) stays the lightweight in-place skim.

### 5.8 Discard worktree

Failed/cancelled/done states include a discard affordance wired to the existing
guarded worktree-removal flow (D4.1 gap 7): drops branch + worktree, keeps the run
record; artifacts (§5.5) survive. No janitor surface in v1.

---

## 6. Sidebar integration

### 6.1 Per-project workflows section

A new `components/sidebar/WorkflowsSection.svelte`, mounted from `ProjectItem.svelte`
below the project's thread list; hidden entirely for projects with no workflows and
no runs.

- **Header row**: chevron + uppercase "Workflows", hint-toned. Right roll-up:
  collapsed with attention → amber dot + amber count; collapsed without → neutral run
  count; quiet → hint-toned count only; expanded → none. Click toggles expansion.
- **Run rows** (expanded): same skeleton/behaviors as `ThreadRow.svelte`; signal
  vocabulary reuses `threadStatusPill.ts` conventions (dot + label + pulse +
  `status-glow-warning`) restricted to the two workflow hues: amber "Needs you"
  (gate/question, pulse + glow), red "Failed". All else typographic: running
  `2/5 · 14m`, queued `queued` (dim), done `done` (dim); cancelled not listed. Order:
  needs-you (oldest first), failed, running, queued, done.
- Row click → workflows pane at that run's detail (§2.2); the row matching the pane's
  current run gets the active highlight.
- No drag-to-pane, no context menu in v1 (runs are not threads).

### 6.2 Global footer badge (D11 amendment 3)

A `WorkflowsFooter.svelte` row in `Sidebar.svelte` directly **above**
`SettingsFooter.svelte`: icon + "Workflows" + the single global needs-attention count
(amber, only when > 0; none when quiet). Click → workflows pane at overview. Always
visible — the global entry point.

### 6.3 Thread-list exclusion (new thread modes)

Phase threads (`workflow`), studio threads (`workflow-studio`), and triage threads —
the D4.9 triage agent AND D4.2/hand-off item triage threads (`workflow-triage`) —
carry new `threads.mode` values (D7) and MUST NEVER appear in normal thread lists,
search results, or thread pickers. Exclusion is principled by mode in
`utils/sidebarTree.ts` / `ProjectThreadList.svelte` / `UnifiedThreadPicker.svelte`,
never by title convention. They are reachable only from workflow surfaces (§3.1,
§5.6, §8) and, once open, behave as completely normal thread panes (`Thread.mode`
union in `types/models.ts` widens accordingly).

---

## 7. Intake

### 7.1 New-run dialog

Opened by `+ New run`. A dialog on the existing Modal primitive
(`components/primitives/`) — the removed modal (D11 amendment 2) is the *inspection*
modal; form dialogs follow existing app convention (`AddProjectModal`,
`ThreadFromPRDialog`). Fields, in order (one run shape from every producer):

1. **Project** — segmented control (dot + name).
2. **Goal** — multi-line text (`work_items.goal`).
3. **Workflow** — picker cards: name, chain, meta (`5 phases · 1 human gate`).
   Definitions failing dry-run validation render greyed-out with their first
   validation error; selection blocked (D4.1 gap 6).
4. **Base branch** — text, prefilled from the project profile.
5. **Typed seed inputs** — the workflow's declared inputs as plain form fields (R2),
   two-column where they fit: string → text (multi-line → textarea), enum → select,
   boolean → checkbox, number → number field; `format: path` gets a file-pick
   affordance. Optional inputs labeled `optional`.
6. **Pause at every gate** — step-mode checkbox (D4.7), default from the workflow
   definition (D4.1 gap 4).

Footer: primary `Queue — position N` (predicted) · `Cancel`. Submit toast "Queued —
position N · starts when a slot frees". Esc closes.

### 7.2 Enqueue from chat (D4.5)

An agent in any interactive thread may propose a run; the proposal renders as a
**confirm card** in that thread's timeline (never in the workflows pane): heading
"Queue this run?", one summary line (project dot · project · title · workflow ·
base), actions `Queue it` (primary) / `Edit` (opens §7.1 prefilled) / `Dismiss`. Chat
NEVER enqueues silently — the card is the commit point. Chat-queued runs are
identical queue citizens (`source: agent`).

---

## 8. Thread break-outs

### 8.1 Phase thread (watch/steer)

Opening a phase thread (§5.3 running, §5.6 historical) opens a normal thread pane —
same status pill, tool rows, composer; zero workflow-flavored chrome. Watching is
passive; sending a message takes over the turn (WHAT-spec §7). Take-over from a
gate/question action row does the same and toasts the mechanic ("Turn interrupted —
the review thread is yours to steer").

### 8.2 Hand-off ("Continue with agent")

One click on failed/done creates a triage thread (mode `workflow-triage`) in the
run's worktree, pre-seeded with run record, envelopes, diff, and typed reason (D4.2)
— surfaced as a single context chip (`⛁ flaky-test-hunt — run record · diff · 3
diagnoses`), never raw internals (R2). The seeded kickoff message sends immediately;
the agent starts; the thread opens as a new focused thread pane. The run resolves in
place: "Continuing with agent — thread opened in the run's worktree". The triage
agent (D4.9) can spawn the same threads from conversation; they surface as openable
cards in its chat.

---

## 9. Keyboard bindings

Registered through the existing keybinding/command registries, scoped to the focused
workflows pane; suppressed while any text field has focus (Esc blurs the field
first).

| Key | Context | Action |
|---|---|---|
| `Esc` | any depth | disarm confirm → close dialog → pop one level (no-op at overview; never closes the pane) |
| `Backspace` | any depth | pop one level |
| `j` / `→` / `↓` | run detail, parked set | next needs-attention run (wraps) |
| `k` / `←` / `↑` | run detail, parked set | previous needs-attention run (wraps) |
| `a` | gate | approve → next phase |
| `a` | question | focus the answer input |
| `a` | failed | re-enqueue with guidance |
| `a` | done (manual) | merge to main |
| `r` | gate | request changes (reveal note input; Enter commits) |
| `r` | failed / done | discard |
| `t` | gate / question | take over the phase thread |
| `t` | failed / done | continue with agent (hand-off) |
| `1`–`9` | question | pick + send the nth suggested answer |
| `Enter` | gate | toggle the first diff file's hunks |
| `Enter` | question input focused | send the answer |
| `Ctrl/Cmd+W` | pane focused | close the workflows pane (standard pane close) |

---

## 10. OS notifications + deep links

- **Per-item**: a run entering `needs-human` (any typed reason) or `failed` fires one
  OS notification — title = run title, body = the one-line "what it needs". Click →
  app foreground, workflows pane at that run's detail, inside the sweep.
- **Coalesced drain summary** (D3.1): on drain-to-empty and on pause — one
  notification summarizing outcomes ("4 finished · 2 need you · 1 failed"). Click →
  the **triage agent** thread pane (D4.9).
- Done-awaiting-disposition alone does NOT notify (R1); it rides the drain summary.

---

## 11. Empty + quiet states

- **Quiet sidebar** (nothing needs anyone): each project's workflows section
  collapses to the single hint-toned header line with a neutral count — no color, no
  badge; the footer badge shows no count. The normal-view test: workflows are
  invisible until they have a reason not to be.
- Overview with no workflows → §3.4 empty state. No live runs → `no live runs` hint.
  No queued or running runs in any project → Queues hidden (§3.3). Sweep entered
  empty → all-clear directly.

---

## 12. Remote posture (v1)

Remote browsers get **view-only** workflows (D4.1 gap 5): all levels render; every
mutating affordance (queue toggle, reorder, intake, action rows, notes editing, Run
now, enable/disable, discard) is disabled with tooltip "Local only". Mutation RPCs
classify `LocalOnlyMethods` in `internal/transport/internalmethods.go`. Remote
gate-approval is explicitly not v1.

---

## 13. Non-goals (normative)

- NO variables / envelope / JSON / schema / gate-trace rendering on any human-facing
  surface — those exist for agents (studio, triage, `ao` CLI).
- NO kanban board page or global work surface — the overview pane is the only
  aggregate view.
- NO inspection modal / slide-over — resolution lives in run detail (D11
  amendment 2). Form dialogs per §7.1 are permitted.
- NO workflow-settings forms beyond §4.1's toggle/Run-now and notes — everything
  richer is studio-agent work over files.
- NO silent enqueue from chat (confirm card always).
- NO remote mutation of any kind (view-only v1).

---

## 14. Log-over-mockup divergences applied

1. History rows open historical run detail with openable phase-attempt threads (D11
   amendment 3); the mockup rendered them inert.
2. Global sidebar footer badge (D11 amendment 3); absent from the mockup.
3. Outputs/deliverables block in run detail (D13); absent from the mockup.
4. Discard-worktree on cancelled/failed (D4.1 gap 7); mockup showed "worktree kept"
   with no action.
5. Step-mode checkbox + greyed-out invalid workflows in intake (D4.1 gaps 4/6);
   absent from the mockup.
6. `+ New workflow`, Edit-in-studio, triage-agent entry, automation enable/disable +
   Run now (D4.1 gaps 1–3); absent from the mockup.
7. The hand-off triage thread does NOT appear in the normal thread list (D4.1
   amendment); the mockup showed it as a normal sidebar thread row.
8. Gate "Request changes" collects an optional feedback note (D5); the mockup
   rejected without one.

## 15. Integration point map

| Concern | Where |
|---|---|
| Pane kind + layout | `stores/paneLayout.svelte.ts` (`PaneLayoutKind` + `'workflows'`), `components/panes/PaneHost.svelte` mount branch |
| Pane persistence | `stores/paneLayoutPersistence.ts` (kind + stack snapshot) |
| Navigation/store | new `stores/workflowsPane.svelte.ts` |
| Events / RPC | new `stores/eventsWorkflow.ts` via `events.ts`; `stores/bindings.ts`; typed `workflow:*` channel (D7) |
| Sidebar section | new `components/sidebar/WorkflowsSection.svelte` from `ProjectItem.svelte`; pill/glow conventions from `utils/threadStatusPill.ts` |
| Footer badge | new `components/sidebar/WorkflowsFooter.svelte` in `Sidebar.svelte`, above `SettingsFooter.svelte` |
| Thread exclusion | `Thread.mode` union (`types/models.ts`), `utils/sidebarTree.ts`, `components/sidebar/ProjectThreadList.svelte`, `components/palette/UnifiedThreadPicker.svelte` |
| PR review | `stores/reviewPane.svelte.ts` + `stores/companionPanes.svelte.ts` (`openCompanion(workflowsPaneId, 'review')`) |
| Keyboard | `stores/keybindings.svelte.ts`, `stores/commandRegistry.svelte.ts` (pane-scoped, palette-target rules apply) |
| Intake dialog | `components/primitives/` Modal shell, sibling to `AddProjectModal.svelte` |
| Toasts | `stores/toast.svelte.ts` |
