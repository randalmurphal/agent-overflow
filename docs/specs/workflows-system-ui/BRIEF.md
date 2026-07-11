# Workflows System UI — Design Round Brief

Three competing information-architecture concepts, one brief. Each concept is a single
self-contained HTML mockup a human will open in browser tabs side by side.

## Required reading (in this repo)

1. `docs/specs/workflows-system.md` — the behavior spec (§2 lifecycle, §6 queue, §7
   human surface, §10 surfaces/notifications are the UI-relevant parts).
2. `docs/specs/workflows-system-decisions.md` — **D3 and D4 override the spec where
   they conflict** (queue is a toggle not a session; merge/PR/discard disposition;
   stepper; diff-first gates; studio; step mode; phase threads never in the normal
   thread list).
3. `frontend/src/app.css` — extract the actual Tailwind v4 `@theme` design tokens
   (colors, fonts, radii, spacing) and mirror them inline so the mockup feels native
   to Agent Overflow. Skim `frontend/src/lib/components/sidebar/Sidebar.svelte` and
   `frontend/src/lib/components/panes/PaneHost.svelte` for structural feel (left
   sidebar + pane row shell). Dark theme is the default lens.

## The seven moments (every concept must render all seven)

1. **Queue overview** — items across states, the active/paused queue toggle,
   concurrency indicator, drag-reorder affordance for queued items.
2. **Running card** — current phase (e.g. "3/5 · review"), live activity one-liner,
   elapsed, cost-so-far, cancel.
3. **Morning-after stepper** — step through parked items one at a time; each shows a
   short digest ("what happened / what it needs") and inline actions:
   approve / answer / take over / re-enqueue / discard. Keyboard-drivable
   (j/k next/prev, a approve, r reject, t take over — actually functional in the
   mockup).
4. **Gate review** — a human gate leading with the worktree diff + check results +
   cost; envelope/variables/narrative demoted to a secondary tab/panel.
5. **Intake** — the explicit form (project, goal, workflow picker, base branch, seed
   variables rendered from the workflow's typed inputs) AND a hint of the
   conversational path ("queue this from chat" confirm card).
6. **Opening a phase thread** — how the user gets from an item to a live phase
   thread (which opens as a normal AO pane; mock the transition, not the thread).
7. **Done disposition** — a done card offering merge / create PR / discard, plus the
   auto-merge-enabled variant (project opted in) showing what "merged automatically"
   reads like after the fact.

## Canonical sample data (identical across concepts — do not invent your own)

Projects: `agent-overflow` (blue), `m32rimm` (orange), `dispatch` (green).
Workflows: `build-and-validate` (5 phases: plan → implement → check → review → docs),
`multi-lens-review` (3 phases: fan-out 3 reviewers → synthesize → fix).

Items:
1. AO · "Fix WebSocket reconnect dropping queued sends" — running, build-and-validate,
   phase 2/5 implement, 14m elapsed, $1.84, live line: "Editing internal/transport/wsclient.go".
2. AO · "Add retention settings UI" — needs-human(gate), review gate, diff +34/-12
   across 4 files, checks green, $3.10.
3. m32rimm · "Refactor session pooling" — needs-human(question), question: "Two pool
   implementations exist (v1 in pool.go, v2 in session_pool.go) — consolidate on v2
   or keep both behind the flag?"
4. dispatch · "Bump Tailwind v4.1 + fix tokens" — done, awaiting disposition,
   checks green, $0.92.
5. AO · "Flaky test hunt in triage router" — failed(retries-exhausted), diagnosis
   classified genuine ×3, $6.40.
6. m32rimm · "Poll Jira → triage new tickets" — queued (from automation `jira-poll`,
   badge it).
7. dispatch · "Migrate config loader to profiles" — queued (manual).
8. AO · "Spike: virtualized diff renderer" — cancelled yesterday.

Queue state: active, concurrency 2/3 slots in use.

## Deliverable constraints

- **One file**: `docs/specs/workflows-system-ui/concept-<x>.html`. Fully
  self-contained: inline CSS/JS, no external requests, no shared assets, works when
  served statically.
- Internal navigation between the seven moments (hash routing or tabs) + a fixed
  header naming the concept so tabs are tellable-apart.
- An **annotations toggle** (ⓘ) overlaying short rationale notes on key IA choices.
- Realistic density — this is a working tool for a keyboard-heavy solo dev, not a
  marketing page. Match AO's compact information density.
- Real interactivity where it sells the concept (stepper keyboard nav, queue toggle,
  tab switches); everything else can be static.

## The concepts

- **Concept A — Dashboard-first** (`concept-a.html`): workflows is a full-page
  surface (the mechanism Settings uses today); board + stepper + detail all live
  there; the existing sidebar gains only a per-project needs-attention badge that
  deep-links in.
- **Concept B — Sidebar-integrated** (`concept-b.html`): each project in the
  existing sidebar grows a collapsed "Work" section (items → expand to phases);
  resolution happens in panes; a board page exists but is secondary.
- **Concept C — Mode switch** (`concept-c.html`): the sidebar toggles between
  *Threads* and *Work* modes; Work mode replaces the thread list with the item tree
  and a persistent queue header (toggle, slots, drain status); the pane row hosts
  detail/stepper surfaces.

The user's own framing of the open question, verbatim: "Definitely dont want them to
show as normal threads, but not sure what the proper UI makes sense. Separate within
the project? Separate side bar entirely? A dashboard instead? Not sure..."
