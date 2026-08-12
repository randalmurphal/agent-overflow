# Workflows System — Implementation Decisions Log

> Companion to `workflows-system.md` (the WHAT spec). That spec stays behavior-only;
> this log records the concrete implementation decisions made while planning the build,
> one topic at a time, in dependency order. Fold behavior-relevant outcomes back into
> the spec when they change the WHAT.

## D1. Workflow definitions: format, storage, layering (2026-07-11)

**Decided:**

- **Format:** structured YAML, one self-contained file per workflow (phases, gates,
  transitions, variable declarations, resource names all inline). Prompts are sibling
  Markdown files referenced by relative path. No separate reusable "phase template"
  layer (orc's 3-layer split is dropped); reuse composes via sub-workflows.
- **Authoring is agent-first.** The YAML shape gets a published JSON Schema (embedded
  in the binary, also written to disk next to the workflow dirs) so agents and editors
  can author/validate without reading engine code. Each scaffolded file carries a
  `# yaml-language-server: $schema=...` header line.
- **Storage: app-managed directories, never in-repo.** Per-project workflows exist but
  do NOT live in the project's git repo (explicit user call; may be revisited).
  - Shared: `<app-config>/workflows/`
  - Project-scoped: `<app-config>/projects/<project-slug>/workflows/`
  - Resolution: project → shared; project wins on `id` collision.
  - The engine reads workflows through a small resolver over an ordered list of
    (directory → scope) sources, so adding an in-repo source later (e.g.
    `.ao/workflows/`) is one more entry, not a redesign.
- **Identity:** a declared `id:` field inside the file (validated unique per scope at
  load), not the filename stem.
- **Disk is the source of truth; SQLite never stores definitions.** The only persisted
  copy is the frozen resolved snapshot in each run record (spec §8 pinning). No
  draft/published lifecycle, no registry table. Edit file → next enqueue picks it up;
  running items keep their snapshot.
- **Scale/multi-user note:** file-based holds for the possible future shared-server
  deployment (the config dir lives on the server host); SQLite storage would only be
  reconsidered for massive workflow counts, which is not the intended use.

**Open follow-ups routed to later topics:**
- `<project-slug>` needs a stable per-project key (likely a new column on `projects`)
  → schema topic (D6).
- Starter workflows (`build-and-validate`, `multi-lens-review`, `poll-jira-and-start`)
  are scaffolds copied into these dirs, not a hidden built-in tier.

## D2. Variables + envelope (2026-07-11)

**Decided:**

- **Types are JSON Schema fragments** (string/number/boolean/enum/array/object), no
  custom type system. Convention: `format: path` strings for file/artifact refs
  (may point at gitignored build outputs).
- **Declarations:** workflow-level `inputs:` (seed variables); per-phase `inputs:`
  (validated refs) and `outputs:` (name → schema). Phase outputs are namespaced by
  phase id (`plan.approach`); workflow inputs are bare names.
- **Interpolation:** `{{var}}` / `{{var.field}}` only. No conditionals, no filters,
  no logic in templates. Single-pass substitution — values are inserted verbatim and
  never re-interpolated (a value containing `{{...}}` is inert).
- **Envelope schema is generated, never hand-written:** system-owned control fields at
  the top level + declared outputs nested under an `outputs` key (no collision with
  control fields by construction). Closed status set:
  `done` (outputs required) | `question` (`question: string` required → park
  needs-human(question)) | `stuck` (`reason: string` required → park
  needs-human(stuck)). Enforced so a phase cannot end a turn without exactly one of
  the three shapes. A question's answer is simply the next turn in the same session
  with the same schema attached — no provider-level suspension mechanic. No fourth
  `failed` status: check failures belong to deterministic check phases + the
  diagnosis phase, not self-assessment; "can't proceed" is `stuck`.
- **Narrative file ref is system-attached** (path dictated in the prompt preamble,
  recorded on the run record), never an agent-filled envelope field.

**Failure modes considered → handling:**

1. *Invalid/missing envelope after provider retries* (Claude
   `error_max_structured_output_retries`, Codex validate-and-retry failure) → §12
   path: park `needs-human(agent-error)` with partial envelope. Never silent.
2. *Engine trusts provider validation* → it doesn't: **fail-closed post-validation**
   in Go against the same schema on every envelope, regardless of provider claims
   (kept from the self-improvement research Layer 0). Invalid after post-validation →
   one feedback-carrying retry turn ("your envelope failed validation: <errors>"),
   then park.
3. *`oneOf` vs constrained decoding* — some decoders handle `oneOf` poorly. The
   adversarial spike tests `oneOf` vs flat-object + `if/then` vs
   flat-object + post-validated conditionals on BOTH providers and picks the shape
   that holds up; engine post-validation covers whichever loses fidelity.
4. *Missing optional variable at interpolation* → renders as literal `(not provided)`,
   never silent empty string. Dry-run rule: a **required** phase input must trace to a
   required producer (workflow required input or upstream required output);
   referencing an optional producer forces the input to be declared optional.
5. *Predicates/inputs referencing fields of absent optionals* → same rule; dotted-path
   refs into optional objects are optional themselves.
6. *Name collisions* → phase ids and workflow input names share one namespace;
   dry-run rejects collisions. Control-field names can never collide (outputs are
   nested).
7. *Oversized envelopes* (agent inlines a huge blob) → hard size cap enforced at
   post-validation (default ~64KB, profile-overridable); over-cap → validation-failed
   feedback retry ("write to a file, return a path"). Keeps run records lean
   (frontend-memory principle 4).
8. *Secret leakage* → secrets are never variables; resolved secret values are masked
   in BOTH the narrative file and envelope strings before persistence (spec §8 only
   said narrative — extended here).
9. *Prompt-hostile seed content* (ticket text with injection attempts) → structural:
   starter-workflow prompts fence untrusted seeds in delimited blocks; single-pass
   interpolation guarantees no template re-execution. Residual risk is inherent to
   agentic work and mitigated by worktree isolation + nothing-auto-merges.
10. *question ping-pong* → each `question` parks for a human; human-bounded by
    construction, no runaway.
11. *Multi-round logical turns* (Claude auto-continue rounds) → which result event
    carries `structured_output` when a logical turn spans wire rounds is explicitly
    in the spike scope.

## D2a. Envelope mechanics — spike verdict (2026-07-11)

Adversarial spike on both providers' streaming modes (claude 2.1.205 stream-json
session; codex-cli 0.144.0 app-server). Raw runs in the session scratchpad
(`structured-output-spike/`). Findings that BIND the design:

1. **Envelope schema shape is settled: flat object, `additionalProperties:false`,
   every property `required`, branch-specific fields typed `["T","null"]`, a
   top-level `status` enum discriminator.** `oneOf`/`anyOf`/`allOf`/`if-then` are
   rejected by BOTH providers (Codex strict mode; Claude tool input_schema
   top-level restriction). Conditional requirements ("question non-null when
   status=question") are enforced by engine post-validation only.
2. **Value keywords are decorative on Codex** (`minimum`, `maxLength`, etc. —
   silently ignored; a value-invalid message COMPLETES the turn). All value
   constraints (including D2's envelope size cap) are engine-enforced.
3. **"Valid-or-explicit-error" is FALSE as the spec states it — on both
   providers.** Claude's mechanism is an *encouraged* synthetic tool call
   (`StructuredOutput`); an adversarial/give-up turn ends `is_error:false` with
   `structured_output` simply ABSENT (the `error_max_structured_output_retries`
   subtype fires only under unusual pressure). Codex is logit-constrained
   (survived the adversarial prompt) but only structurally. **Engine invariant:
   the ONLY success signal is a present payload that passes the engine's own
   full-schema validation. Absent-or-invalid = envelope-production failure →
   one feedback retry turn, then park (per D2).** Never gate on
   `is_error`/`subtype`/turn-status alone. Spec §10's wording gets corrected in
   the fold-in.
4. **Wiring:** Claude — `--json-schema '<inline JSON>'` (inline string, NOT a
   path) in `buildArgs`; coexists with AO's full flag set; sticky for the whole
   session; payload at `result.structured_output` (pre-validated by the CLI).
   Codex — `outputSchema` (camelCase) in `turn/start` params; **must be re-sent
   every turn** (not sticky, despite upstream docs); payload = JSON-parse of the
   final `item/completed` agentMessage `item.text` (NOT on `turn/completed`).
5. **Takeover/finalize fits cleanly:** Codex free-form turns are naturally
   schema-less (just omit `outputSchema`); the finalize turn re-sends it. Claude
   can't unregister the schema mid-session, but since the mechanism is an
   encouraged tool, free-form turns behave normally — acceptable.
6. Caveat: spike ran on small models (haiku / gpt-5.4-mini) to bound cost; the
   *mechanisms* (flag→tool registration, OpenAI strict mode) are
   model-independent, but re-confirm envelope-absence rates on the real phase
   models during M1 integration testing.

## D3. Product rulings (user decisions, 2026-07-11)

1. **Queue is a toggle, not a session.** The queue is active/paused; while active,
   enqueued items start when a slot frees. "Process N then stop" survives as an
   optional bound. "Run" stops being a first-class session entity; the run *record*
   remains per-item. Coalesced summary notifications fire on drain-to-empty (and on
   pause). Manual priority = simple drag reordering of queued items (UI round owns
   the affordance). **Amends spec §6.**
2. **Scheduler/automations (§11) ship in v1.** Cron + internal-event triggers +
   authenticated-CLI polling with cursors. Not a fast-follow.
3. **In-app disposition in v1: merge / PR / discard on done items.** Net-new local
   merge machinery. **Spec amendment (user call): "nothing auto-merges" becomes the
   default, not an absolute** — a project profile may opt into auto-merge-on-done
   (side projects); production projects stay manual. Auto-merge proceeds only on a
   clean merge from a green terminal state; any conflict/dirty base refuses and
   parks `needs-human(disposition)` with a pre-seeded triage thread ("cleanly
   resolvable" — never forced, never silent). **Amends spec §9.**
4. **Phase threads never appear in the normal thread list.** Their home (separate
   per-project section vs separate sidebar mode vs dashboard-only) is deliberately
   unresolved — the UI/UX design round explores it as its centerpiece question.

## D4. Adopted product requirements (from the product review, 2026-07-11)

Accepted as v1 product behavior; each becomes spec text in the final fold-in:

1. **Workflow studio** — agent-native authoring/editing: a mode/skill granting an
   agent the workflow JSON Schema, the dry-run validator as a tool, and R/W access
   to the app-managed workflow dirs + project profiles. Entry points: create new,
   edit existing, and "open in studio" from any run (pre-loads run record + frozen
   snapshot).
2. **Item-level triage threads** — every failed/parked card offers "open a triage
   thread" pre-seeded with run record, envelopes, diff, and typed reason, in the
   item's worktree. (Phase-level problems keep §7 takeover of the phase thread.)
3. **Needs-attention stepper** — the throughput surface: step through parked items
   one at a time, each with a short generated digest ("what happened / what it
   needs"), with approve / answer / take over / re-enqueue / discard inline. The
   board is orientation; the stepper is resolution.
4. **Gate approvals lead with the diff** — human-gate detail is a review surface
   (worktree diff, check results, cost) first; envelope/variables/narrative second.
5. **Enqueue from anywhere** — interactive chat threads get the same enqueue tool
   phases get ("queue a fix for this"), shaping items from conversation context
   with a user confirm.
6. **Live pulse on running cards** — current phase, live activity line, elapsed,
   cost-so-far (usage_ledger) on every running item card.
7. **Step mode** — per-item run option forcing a park at every gate (all gates
   behave as human gates); the trust-building mode for new workflows.
8. **Read-only phases** — phase-level `access: read-only | write` mapped to
   provider sandbox flags; review/judge/diagnosis phases default read-only.
   Read-only fan-out units may share the primary worktree (serial-writer constraint
   applies to writers only).

9. **Triage agent (user addition, 2026-07-11).** An interactive agent the user can
   open (or that the drain-summary notification deep-links into) whose job is
   "figure out what needs my attention and set up the conversations for it."
   Granted first-party tools over the workflow domain:
   - *Introspection (read-only):* list items/states/typed reasons, read run
     records, envelopes, narratives, diff summaries, costs.
   - *Spawn seeded thread:* create a triage thread for an item (same seeding as
     D4.2 — run record + envelopes + diff + typed reason — plus an optional
     framing note distilled from the concierge conversation, e.g. "user leans
     v2 for the pool consolidation"). The spawned thread surfaces as an openable
     card in the concierge chat; user clicks in and discusses there.
   - *Actions:* enqueue / re-enqueue (D4.5's tool), gate approve/reject — action
     tools surface AO's normal interactive approval UX (the workflows "no
     per-call HITL" rule governs headless phases, not interactive chats, so
     confirming a gate decision in-chat is consistent).
   Scope call: v1 covers the workflow domain only (items + phase threads);
   app-wide attention (ordinary threads, PRs) is a later extension the toolset
   design must not foreclose. The stepper remains the zero-agent fast path; the
   triage agent is the conversational path over the same data. UI entry point
   gets added during the redline round on the winning design concept (the brief
   won't be amended mid-render for A/B).

**Scope calls made by Claude (veto if wrong):**
- **Agent-invoked inter-agent discussions (§5 tool) defer to post-v1.** Additive,
  not structural; v1 already grew by the scheduler. Human-initiated discussion
  flows and all other §5 tools (enqueue, report-back, schedule, query-source)
  stay v1.
- **Fan-out/fan-in ships v1** (multi-lens-review starter needs it);
  **sub-workflow phase shape defers to fast-follow** (nothing in v1 forecloses it).
- Report-back/query-source bind to *profile-declared commands* (like check
  commands), not hardcoded CLI names — the engine never assumes a specific tool.
- **Jira integration (user ruling, 2026-07-11): do NOT depend on the user's
  personal `jira-*` scripts** (they may be removed). Starter content and docs
  target the official Atlassian CLI (`acli`) — verified adequate: GA for Jira,
  `workitem search --jql ... --json --fields ...` covers polling with an
  `updated >= <cursor>` watermark; workitem view/edit/comment/transition cover
  report-back — and/or the Atlassian remote MCP server for agent-driven phases
  (grantable via profile MCP config). `gh`/`glab` stay as-is for GitHub/GitLab.
  Building our own Jira CLI is a fallback only if `acli` proves inadequate for
  the narrow poll/report surface; not planned.

## D5. Gates + transitions syntax (2026-07-11, delegated to Claude)

- A gate is the phase's `gate:` block: an **ordered route list, first match wins**.
  No free-floating gate nodes. First phase in the list is the start node; `done`
  and `failed` are reserved terminal targets.
- **Predicates are structured YAML** (never string expressions): `eq, neq, gt, gte,
  lt, lte, in, exists` + `all, any, not` combinators. Nothing else (no regex —
  string-shape decisions belong in a phase). Ordered, short-circuit evaluation.
  Predicates over absent optionals evaluate false; only `exists` observes absence.
- **Loop-back**: `loop: <ancestor-phase>` + mandatory `max: N` + optional
  `feedback: [vars]` (plus free-text note on human rejects). Counters are per
  gate-edge, persisted in the run record. Exhausted `max` falls through to the next
  route; list exhausted after a loop-exhaustion → park
  `needs-human(retries-exhausted)`.
- **Runtime no-match** (list exhausted, nothing matched, no exhausted loop) → park
  `needs-human(wiring-error)` with the evaluated predicate trace persisted. Never
  crash, never silently advance.
- **Human gates are a route kind** (`human: {approve: X, reject: {loop: ...}}`);
  hitting one parks `needs-human(gate)`; card buttons route directly; rejection
  note rides as feedback.
- Dry-run checks: targets exist, reachability, loop targets are ancestors,
  loop routes carry `max`, every `when` reference has a producer that dominates
  this phase in the graph (else must be declared optional), no type-mismatched
  comparisons.

## D6. Project profile (2026-07-11)

- **Storage:** `<app-config>/projects/<slug>/profile.yaml` — same not-in-repo rule
  and same loader/validator infrastructure as workflows (D1); agent-editable via
  the studio. Project *identity* stays the DB `projects` row; `projects` gains a
  stable unique `slug` column.
- **Contents:** default base branch; check commands (name → argv); resource
  capacities (name → int); report-back/query-source command bindings (name → argv
  template — e.g. `acli`, `gh`, `glab`; never a hardcoded tool); reliability defaults (watchdog T,
  default per-item budget); disposition policy (`manual` | `auto-pr` |
  `auto-merge`, default `manual` per D3.3); secret references (v1: `env` and
  `file` sources; OS keychain later); MCP servers grantable to phases.
- Dry-run cross-check: every check/resource/command a workflow references by name
  must be bindable by ≥1 profile (spec §3 rule, enforced here).

## D7. Engine placement + architecture (2026-07-11)

- New package family: `internal/workflow/def` (YAML parse/resolve/validate +
  embedded JSON Schema), `internal/workflow/engine` (queue drain, item/phase FSM,
  teardown, watchdog, resource semaphores), `internal/workflow/wftools`
  (first-party toolset backend). Bare CRUD stays in `internal/store` per the
  project/store split precedent.
- **Follows the discussion-FSM precedent:** in-memory scheduler state rebuilt from
  SQLite at startup; a single engine goroutine owns state transitions; store
  writes are short-lived calls (respects the single-connection DB).
- **Refactor (committed):** a generic turn-observer registry on `App` replaces the
  bespoke per-feature `if evt.Kind == EventTurnComplete` branches in
  `app_provider_events.go`; discussions migrate onto it; the engine and the
  watchdog both consume it.
- Phase threads get a new `threads.mode` value `workflow` (Rebuild migration) so
  their exclusion from normal lists is principled, not filtered by convention.
- Transport: new bound methods classified `LocalOnlyMethods` (session-control
  class); state changes ride a new typed `workflow:*` channel (no generic
  passthrough), following the documented add-a-channel path.
- CLAUDE.md updates at implementation start: Core Principle 1 gains the workflow
  engine as the second named coordination exception; the "Deferred" entry is
  removed.

## D8. Persistence shape (final DDL at implementation; migrations v21+)

- `work_items` — id, project_id, goal, workflow id+scope, **frozen snapshot JSON**,
  state CHECK (queued/running/needs-human/done/failed/cancelled), typed reason
  CHECK, sort_position (drag-reorder), seeds JSON, worktree/branch/base,
  budget, source CHECK (manual/agent/automation) + source_ref, timestamps.
- `work_item_phases` — one row per phase *attempt*: item_id, phase_id, attempt,
  thread_id (denormalized, no FK — records outlive threads, usage_ledger
  precedent), input/output envelope JSON, gate trace JSON (evaluated predicates +
  route taken), intervention JSON (§7 takeover record), narrative path, timings,
  status. Loop counters derive from these rows; no separate counter state.
- `work_item_effects` — A5 surface-and-skip ledger: (item, phase, tool,
  payload_hash) → effect record.
- `automations` + `automation_cursors` — trigger (cron/event), condition, action
  (workflow + seed template), per-source cursors.
- `usage_ledger` gains nullable denormalized `work_item_id` (per-item budget sums).
- Retention: run-record tables are never cascade-deleted by thread/project
  cleanup; deletion of workflow history is its own explicit action.

## D9. Reliability wiring (2026-07-11)

- **Teardown is one engine function** (spec §12 contract): stop turn → release
  resource semaphores → persist partial envelope on the phase row → transition
  with typed reason → emit `workflow:*` event + notification. All five triggers
  call it; it is the only path that touches locks.
- Watchdog: per-active-phase timer reset by any provider event for the phase
  thread (fed by the D7 observer registry). Default T = 15m, profile default,
  per-phase override.
- Transient retry allowlist (conservative, per spec A10): session death
  (`EventSessionStatus: error`), subprocess exit before any envelope, and
  explicit provider overload/network error classes. Backoff 30s → 2m → 5m, cap 3,
  then park `needs-human(retries-exhausted)`. Everything else parks immediately.
  **Distinct from D2a's envelope-invalid path** (one feedback retry turn → park
  `needs-human(agent-error)`).
- Budget: checked at phase boundaries via `usage_ledger` sum over `work_item_id`
  (includes takeover/finalize turns — they run in phase threads).
- Startup sweep (RecoverCrashedTurns pattern): items `running` whose current
  phase attempt has no terminal envelope → teardown → park
  `needs-human(interrupted)`.

## D10. Milestones + delegation (DRAFT — for user sign-off)

- **M0 — Foundations/refactors:** turn-observer registry; MCP config composition
  unification (fixes `--strict-mcp-config` exclusivity); `projects.slug`;
  Wails notification service registration (+ the held notifications spike on the
  real Windows/WSL launcher). *Codex packets; Claude review.*
- **M1 — Definitions:** `def` package (schema, parser, resolver, full dry-run
  lint), profile loader, scaffold. *Highly delegable; pure Go, test-heavy.*
- **M2 — Engine core:** queue drain + item FSM, phase runner (D2a envelope wiring
  both providers), gates, teardown/watchdog/retry/budget, worktree-per-item,
  step mode, migrations. *The heart: codex lanes with tight packets; Claude owns
  architecture + integration tests + review.*
- **M3 — Surfaces:** winning design concept implemented (board/stepper/sidebar/
  intake/gate review), `workflow:*` channels, notifications, disposition
  (merge/PR/discard). *Fable-assisted UI where taste matters; codex plumbing.*
- **M4 — Tools + agents:** first-party MCP toolset (enqueue, introspection,
  spawn-seeded-thread, report-back, query-source), workflow studio, triage agent
  (D4.9).
- **M5 — Automations:** scheduler (cron + internal events), pollers + cursors,
  starter workflows, coalesced summaries.
- Every milestone leaves `make check` + `make test` green; each delegated packet
  gets codex-review + Claude adjudication before merge; spec fold-in (amendments
  from this log → `workflows-system.md`) plus the UI spec extracted from the
  winning mockup happen at M0 start.

## D11. UI direction — design-round verdict (user redlines, 2026-07-11)

All three concepts rejected aesthetically ("too busy, too many badges; attention
isn't clearly drawn"); their *ideas* survive. The synthesized direction:

- **Positioning (reframe, governs everything):** MOST work stays in normal threads
  (human discusses, agent works/orchestrates). Workflows are **background jobs** —
  scheduled/triggered automations and custom tasks (research runs, Jira backlog
  triage). The surface is sized as a jobs system, not the center of the app.
- **Aesthetic:** minimalist and calm; one clear "needs you" signal from the normal
  view; enough info to dig in, no more. Reuse the existing thread-row visual
  language and behaviors wherever they fit. Concept A judged least cluttered
  (still not loved) — restraint is the bar.
- **IA (mix-and-match):** per-project **separate sidebar section** (from B);
  **board layout from A**, but living as a **persistent overview pane** — a
  normal multi-pane citizen, NOT a global surface takeover ("single work surface,
  integrated; the info exists in a pane"); **inspection is a modal**, not a
  slide-over. The modal absorbs the stepper role (prev/next across
  needs-attention items).
- **Phase threads:** sidebar item expands (dropdown) to open the running phase
  thread as a *normal thread* — watch, steer, and continue that phase to
  completion in place. On done/stuck, a one-click hand-off gives the context to
  an agent to continue (D4.2 triage threads / D4.9 spawn tool — re-affirmed by
  user).
- **PR integration:** from the work surface, view/open the item's PR, create
  review comments, send them to the agent — adapting AO's existing PR review
  functionality, not new machinery.
- **No internals in human surfaces.** Variables, envelopes, JSON, gate traces
  never render in the human UI — they exist to make workflows manageable *by
  agents* (studio/triage). Human phase detail = narrative summary, diff, checks,
  cost.
- **Workflow settings live in their own settings area** (rarely changed):
  concurrency, per-project capacities, notification prefs — a UI over the
  profile files, not a new store.

**D11 amendment (user redlines on the synthesis, 2026-07-11):**
- **The overview pane is workflow-centric**, not item-centric: it lists
  *workflows* with their aggregate state — running / queued / **scheduled (next
  run, from automations)** / needs-attention. Items are runs *of* a workflow.
- **Per-workflow detail pane** (a normal pane, master-detail): the selected
  workflow's live runs + run history + next scheduled run + job notes (D12's
  natural home) + per-run phases, with phase threads openable directly from
  within that view.
- **Terminology stays "workflows"** everywhere — most workflows *are* jobs, but
  the surface is never labelled "jobs".
- The inspection modal (j/k quick-resolve across needs-attention) is retained as
  the fast path; the per-workflow pane is the browsing/history path.
- Ratified from the designer's flags: intake renders typed seeds as plain form
  fields (no "variables" vocabulary); overview pane gets a responsive
  single-column collapse at narrow widths; done-awaiting-disposition joins the
  modal cycle and counts neutrally in the sidebar (never amber; no time-based
  escalation for now).

**D11 amendment 2 (user, 2026-07-11): single-pane drill-down; modal removed.**
- The workflows surface is ONE pane with internal stacked navigation + back
  button: **overview (workflow list) → workflow detail (runs/history/schedule/
  notes) → run detail (phases, digest, diff/checks/cost, actions)**. Only
  threads break out — a phase/triage thread always opens as a new normal
  thread pane.
- **The inspection modal is removed.** Its resolution role moves into run
  detail: action row (approve/reject/answer/hand-off/disposition) plus
  lightweight next/prev across needs-attention runs (keyboard preserved), so
  morning-after throughput survives without a separate surface.
- Naming note: the user's mental model is "workflow type" (definition) vs
  "workflow" (a run). UI avoids labeling levels; code/spec vocabulary stays
  workflow = definition, run = execution.

**D11 amendment 3 (user accepts direction, 2026-07-11; mockup iteration DONE).**
No further mockup validation rounds. Two additions recorded for the UI spec, not
re-mocked:
- **Historical phase threads are openable.** Run detail exposes the thread of
  EVERY phase attempt (completed, failed, superseded retries), not just the
  actively running one — run records keep denormalized thread ids precisely for
  this.
- **Global workflows badge in the sidebar footer** (above Settings, the Concept-A
  entry mechanism): opens the workflows overview pane; carries the single
  needs-attention count.
- The mockup is direction, not chrome: real implementation integrates with
  existing surfaces (full diff review opens the actual ReviewPane as its own
  pane; native component styles/theme throughout).

## D13. Run outputs / deliverables (user topic, 2026-07-11)

Workflows whose product is a document/answer, not a branch (reports, research,
triage summaries):

- A workflow may declare workflow-level `outputs:` — named values and/or artifact
  files, sourced from phase outputs. These are the run's **deliverables**, distinct
  from the narrative (process log).
- **Artifact files are copied into an app-managed per-run artifact store**
  (`<data-dir>/runs/<run-id>/artifacts/`) at the producing phase's completion —
  so deliverables survive worktree discard (prerequisite for D14 auto-cleanup).
- **Human access:** run detail lists outputs; artifacts open from the UI
  (preview for md/html, system opener otherwise).
- **Agent access:** the introspection toolset gains `get-run-output(run)`; and the
  enqueue tool gains an optional **post-back-to-origin-thread** flag — when the
  run terminates, the requesting chat thread receives a message with the outputs
  (summary + refs), riding the internal-event machinery. So "agent asks for a
  report" is: enqueue(post_back) → keep chatting → result arrives in-thread.

## D15. Agent interface is CLI-first, not MCP (user ruling, 2026-07-11)

The first-party agent surface for the workflow system is a **CLI** (short binary,
working name `ao`) speaking the existing loopback HTTP+WS transport with scoped
tokens — NOT an injected MCP toolset. Rationale: agents background commands, poll,
and read files natively; MCP calls block and trap results in-context.

- Core shape: `ao run start <workflow> [--seed k=v ...] [--json]` returns the run
  id immediately (non-blocking); `--wait` blocks and prints outputs on stdout when
  the caller wants sync-with-result in ONE command; `ao run wait <id>` is the
  backgroundable variant (agent backgrounds it, gets re-invoked on completion);
  `ao run status|output|list`, `ao workflow list|validate`, `ao notes get|set`.
  `ao run output <id>` is the "different context that didn't start the run" path.
- **Phase grants become token scopes:** §5's "tools granted to a phase" maps to
  which `ao` subcommands the phase's injected credentials (AO_ENDPOINT/AO_TOKEN +
  run context, set at phase entry) authorize — cleaner than MCP allowlists. Effect
  recording (A5 surface-and-skip) happens server-side keyed by the token's
  (run, phase) context.
- Governance is unchanged: `ao` invocations from interactive threads flow through
  the provider's normal bash-approval UX (already surfaced in AO's UI); phase
  tokens are scoped to what the workflow granted.
- Consequences: D4.9's introspection/spawn-thread and D13's `get-run-output`
  become CLI subcommands; D7's `wftools` package becomes CLI + server-side
  handlers; the studio agent works with files + `ao workflow validate`; D13's
  post-back-to-thread flag is retained as the human-visibility nicety, but
  background `ao run wait` is the primary agent pattern. The design-mode loopback
  MCP server stays as-is for design mode; the workflow system just doesn't extend
  it (the `--strict-mcp-config` composition refactor drops out of M0 scope unless
  design mode needs it independently).

## D14. Per-workflow workspace + cleanup policy (user ruling, 2026-07-11)

- **Workspace need is derived, not declared:** if no phase in the graph has
  `access: write` (D4.8), the run gets **no worktree** — it executes read-only
  against the project root. Report/research/triage workflows stop paying the
  worktree tax entirely (their scratch product goes to the D13 artifact store,
  never the repo).
- **Cleanup:** `cleanup: auto | manual` (default manual). `auto` = worktree
  discarded at terminal state after D13 artifacts are captured. v1 rules:
  `auto` always allowed for derived-read-only workflows (no worktree — nothing
  to clean; the setting is inert); for writing workflows `auto` only takes
  effect after a successful disposition (auto-pr / auto-merge landed) — an
  implementing run's unlanded branch is never silently discarded. **Amends
  spec §9's "worktree persists through every terminal state."**

## D4.1 amendment (user, 2026-07-11)

Studio threads (and triage-agent threads) are **excluded from the main thread
history/sidebar** like phase threads — their own mode, reachable from the
workflow's row/detail ("Edit"/"History" affordances) and the overview page, never
mixed into normal chats.

**Coverage gaps identified at sign-off + dispositions (Claude calls):**
1. *Studio entry points:* "+ New workflow" on the overview and "Edit" on workflow
   detail open a studio thread (D4.1); no dedicated editor surface.
2. *Automations management:* minimal in-pane — workflow detail shows trigger
   (cadence/event), enable/disable, next-run, and a **Run now** button; anything
   richer (changing cron, seeds, conditions) is studio-agent work over the
   automation config, not forms.
3. *Triage-agent entry (D4.9):* one affordance in the overview pane header +
   the drain-summary notification deep-links into it.
4. *Step mode:* a checkbox on intake ("pause at every gate") + a per-workflow
   default in its definition.
5. *Remote access posture:* workflow mutation methods classify LocalOnly like
   the session-control methods they wrap → remote browsers get view-only
   workflows v1, consistent with AO's existing remote posture. Remote
   gate-approval is a possible later relaxation (decision + DB write only,
   deferred finalize), explicitly not v1.
6. *Invalid definitions at intake:* the workflow picker lists broken definitions
   greyed-out with their first validation error; enqueue is blocked.
7. *Failed/cancelled worktree cleanup:* run detail's action row includes
   discard-worktree (existing guarded removal); no separate janitor surface v1.

## D12. Job continuity notes (user addition, 2026-07-11)

Scheduled/triggered jobs get **per-job notes** for cross-run continuity (markdown
blob): visible + editable in the item/automation UI, injected as a reserved seed
variable, and optionally rewritten by a terminal phase via a first-party
`update-job-notes` tool (NOOP is a normal outcome). Scoped deliberately small —
a notes file per job, not a memory subsystem (the §12 exclusion stands).

## D6 amendments (2026-07-11)

- **Worktree setup:** profile gains `worktree_setup: {copy: [...], run: [...]}` —
  files copied from the main workspace (e.g. `.env`) and commands run (e.g.
  `uv sync`) at worktree creation, before the first phase. Setup failure parks
  `needs-human` (infra), never starts phases on a broken tree.
- **Locks are per-project by default:** a resource name is an instance within its
  project's profile; the same name in two projects never contends. App-level
  global resources (a machine-wide GPU, say) are a possible later addition, not
  v1.

## M4 rulings (user decisions, 2026-07-13)

- **D4.5 amendment — enqueue-from-chat is user-directed, not autonomous.**
  The producer is a **first-party tool exposed to interactive threads**
  (normal chat + triage) via the existing per-provider MCP plumbing —
  self-describing, so the agent knows the capability exists, but invoked
  **when the user asks**: the tool description must instruct agents not
  to enqueue unprompted. A settings toggle gates the whole feature; off
  means the tool is absent from the session, not merely hidden. The §7.2
  confirm card remains the commit point — chat never enqueues silently.
  (§5's "first-party agency is the `ao` CLI" ruling governs workflow
  *phases* and is unchanged; interactive threads use the tool surface
  their providers already support.)
- **D11 amendment — PR follow-ups become threads.** The bespoke
  "Send comments to the agent → run returns to Running with a fix turn"
  loop is dropped. Instead the PR block gains: (a) **Review comments
  (N)** — fetched review-comment threads seeded into the run's linked
  thread, which already carries the work's context; (b) **Discuss this
  PR** — opens/reuses that thread seeded with PR context to review and
  prepare discussion topics. Both ride the existing done→thread
  hand-off machinery.
- **D3.1 amendment — queues are per-project entities.** The Up-next
  surface renders a *list of per-project queues* (separate lists in the
  column UI), never one interleaved strip. Each project queue is FIFO
  within itself (manual reorder stays per-queue) and gains its own
  **pause toggle** and **concurrent-run limit** alongside the global
  ones. Per-project drain summaries are hereby ratified in §10 — they
  match the engine's structure.
- **Hand-off context ruling (resolves F4.4).** Seeds handed to a
  triage/linked thread carry **intent context** — goal, digest
  (what happened / what it needs), narrative and decisions — never
  diff summaries or file lists. The agent reads code from the worktree;
  the seed's job is "what was this work for and where does it stand."
- **`workflow:definitions-changed` ratified.** Emitted when a
  definition file is written through the app (studio save path);
  sidebar/pane refetch the catalog on it. Deferred from P4.0/P4.1 only
  by packet discipline (no new event channels mid-pass).
- **Standing mandate:** polish, efficiency, and implementation-quality
  improvements are always in scope for future packets — flag them in
  reports, don't sit on them.

## Rev-2 rulings (user decisions, 2026-07-24)

Spec revision 2 (`workflows-system.md`) re-centers the system on
directly-started background runs. The rulings below supersede where noted;
everything else above stands.

- **D16. The queue is removed** (supersedes D3.1 and its M4 per-project
  amendment; queue portions of D4/D7). Runs start immediately through one
  start path shared by the overlay, the `ao` CLI, phase grants, and the
  scheduler. No `queued` state exists — contention is a *phase* waiting on
  resource capacity. Bounded parallelism moves to an **implicit
  `provider:<name>` resource** every agent phase/unit acquires, capacity in
  the project profile, read live at each acquisition. One **global pause**
  survives as the kill switch (no new phase starts; in-flight turns finish) —
  it carries no ordering and no per-project variant. Scheduler actions start
  runs directly with **skip-if-running** overlap policy, recorded per
  automation. Drain summaries die with the queue.
- **D17. CLI everywhere; thread binding + wake** (supersedes the M4 D4.5
  chat-enqueue MCP tool and the P5.0 proposal/confirm flow; extends D15 to
  interactive threads). Interactive threads use the same `ao` CLI as phases,
  under normal bash approval — no MCP server, no proposal card, no pre-start
  confirmation. Context is injected on demand by the **`/workflow` composer
  command** (binary path, project workflow dir, command cheat-sheet, active
  runs) — nothing workflow-shaped sits in context until invoked. Every root
  run records an optional **bound thread**: agent-started runs bind their
  origin thread and **wake it** on every resting state via the existing
  queued-user-message path (tool-boundary delivery); unbound runs surface in
  the overlay, where **open-in-thread** (seeded, then bound) or binding an
  existing thread upgrades them to the same wake behavior. Child runs never
  bind and never notify.
- **D18. Call phases** (supersedes "sub-workflow — post-v1"). A phase may
  invoke a workflow by **static id** (never a variable — the dry-run
  validates the whole call graph). The child is a **real run** — own item,
  phases, counters, record — linked into the parent's tree. **Workspace
  flows down the call stack**: children execute in the caller's workspace;
  isolation is introduced only by fan-out. Cyclic call graphs require
  `max_depth`; budgets are enforced against the **root** item across the
  tree. Each invocation freezes the child's definition at call time.
  Recursion with an exit condition is the sanctioned batch-loop shape: fresh
  child per iteration, fresh loop counters by construction.
- **D19. Fan-out executes** (was parsed-but-rejected). Units: static list or
  **dynamic `over:` an array variable with `as:` binding**. Per-unit
  provider/model/access (mixed-provider fan-outs are intended). Each unit =
  own thread; writing units = own sub-worktree on a real branch, registered
  in the run record. **Unit failure**: stop launching, let in-flight units
  finish, park `needs-human(unit-failed)` with the failed unit's thread +
  narrative; recovery = retry unit / drop unit (join proceeds over
  survivors, recorded) / take over. Dry-run **reports** a static fan-out
  wider than the binding profile's provider capacity.
- **D20. Two join patterns; no silent merge** (relaxes rev 1's "the join
  never git-merges"). Synthesis join (explore-and-synthesize) unchanged.
  **Merge join**: a tool phase git-merges unit branches into the item's
  branch, emitting typed outputs (`clean`, `conflicts[]`); its gate routes
  conflicts to a resolution **agent** phase, bounded, then human. Explicit
  user ruling: **no path-ownership metadata, no join-owned file
  declarations** — enumerating no-touch files is unmaintainable and actively
  harmful; the agent resolution loop is the design. Disposition remains the
  only place the project's base branch is touched.
- **D21. Loop bounds are per fresh entry** (supersedes cumulative per-item
  counting). A loop edge's counter counts consecutive traversals since its
  target was last entered from outside the cycle; derived from persisted gate
  traces, nothing new stored. Takeover attempts still don't consume budget.
- **D22. `access` is enforced** (was derivation-only). `read-only` maps to
  the provider's restricted runtime mode; `write` gets full access inside
  its own isolated workspace. A read-only phase physically cannot dirty the
  project root.
- **D23. Pause / discard / shutdown join the teardown contract.** Pause =
  interrupt in-flight turn(s) → teardown → park `needs-human(paused)`;
  resume = next attempt on the same provider thread (question-answer
  mechanics). Graceful quit pauses every active run; a crash parks
  `needs-human(interrupted)` — same resume, distinct reason. **Discard
  shows a loss preview first** (per worktree in the tree: branch, dirty
  files, unmerged commits) — the preview is the consent. Teardown is
  **tree-aware**: children and in-flight units always come down with the
  root.
- **D17 amendment (2026-07-25).** "Child runs never bind and never notify"
  holds, but it left a subtree parked on a question with nothing on any
  surface. A descendant parking `needs-human` while the root still waits now
  produces the **root's** wake and OS notification, composed to name the parked
  descendant (run id, workflow, typed reason, parked phase). The child still
  never surfaces as itself — the root is the unit of attention, and it is the
  root's thread that is woken.
- **D2a amendment (2026-07-24).** Envelope schemas must be provider-legal
  under both CLIs' strict modes; the rules live in
  `internal/providerschema` (each with its observed CLI rejection), the
  generator obeys them, and the mock provider enforces them so the harness
  cannot drift from reality. Branch rules the engine enforces are stated
  verbatim in the prompt suffix. The envelope-validation retry count is a
  **profile knob (default 1)**, no longer a hardcoded bool.
- **D11 amendment — the overlay replaces the pane + sidebar section.** The
  workflows surface is a full-surface **overlay rendered as a sibling of the
  pane host** — panes never unmount (explicitly not the settings pattern).
  One workflows button in the sidebar footer carries the needs-attention
  badge. No per-project sidebar section, no workflows pane, no deep-link
  pane machinery; OS notifications deep-link into the overlay. Surface
  detail: `workflows-system-ui/UI-SPEC.md` rev 2.

## Ratification sweep (W12, 2026-07-25)

The rev-2 campaign (M6, waves W1–W12) shipped. These five questions were
raised during implementation and deferred to the close; each is settled
against the code as it stands, with the file that carries the behaviour named
so a future reader can check the ruling rather than trust it. D25 was the one
left open at the sweep; it was decided by the user the next day, implemented
once too destructively, and reworked the same day. It is recorded below as the
verdict it now is, with what the first attempt got wrong.

- **D24. `report-back` stays out of the closed grant set** (ratifies the
  deferral in D15/§5). The enforceable grants are exactly `start-run`,
  `schedule`, `update-notes`, and `introspect`
  (`internal/workflow/def/grants.go` `grantSet`), and each maps to methods in
  `transport.ScopedTokenMethods` (`internal/transport/scopedtoken.go`) that
  the app can actually authorize. `report-back` has no such mapping: its
  shape depends on profile-bound forge commands that rev 2 did not settle.
  The set is *closed*, so an unknown name is a validation finding naming the
  available grants — never an ignored declaration — which means admitting
  `report-back` early would let a workflow declare a capability nothing can
  honour and discover it only at run time. Adding it later is additive: a
  new `Grant` constant, its `ScopedTokenMethods` rows, and the bound method's
  row-level check. **Ratified as-is; not a gap.**

- **D25 (RESOLVED 2026-07-25). Deleting a project cleans up after itself; it
  never deletes a branch.** The M6 sweep left this open, and the first
  implementation went further than the ruling that followed it — see the rework
  note below. Settled shape: deleting a project removes what Agent Overflow
  owns — its threads, its runs and their phase/unit/effect rows, its
  automations, its attachments, the project row — and cleans up the litter it
  made on disk, the run worktrees it created in an app-managed directory
  (`<repo>-worktrees`, or `<configDir>/worktrees/<project>`). It deletes no
  branch, so every commit those runs produced is still reachable in the
  repository afterwards. The rejected alternative was leaving the leak in place
  with a reclaim path bolted on later: `work_items` carries no foreign key to
  `projects`, so the runs survived a deletion with a project id resolving to
  nothing, unreachable from every project-scoped query, and their registered
  worktrees stayed in `git worktree list` with nothing left in the app that
  could ever remove them.

  Shape:

  - `App.DeleteProject(id)` (`app_projects.go`) is the one entry point. It
    cancels the project's live runs and stops their provider sessions, removes
    their worktrees, deletes the threads and every workflow row, refreshes the
    automation schedule, and deletes the project. It returns a
    `ProjectDeletionResult`: the thread ids the frontend purges pane state for,
    and the worktrees still on disk afterwards.
  - Removal is `gitCore().RemoveWorktree` — **non-force**, deliberately
    (`app_project_delete_cleanup.go`). Git refuses to remove a worktree
    carrying uncommitted or untracked work, and that refusal is the whole
    safety valve: the app created the directory so it cleans it up, but it does
    not get to destroy what the user left inside it. A refused checkout is a
    reported outcome, not a failure — the deletion still succeeds, and the
    checkout rides back in `RetainedWorktrees` with a reason, so a partial
    cleanup is never a silent one. `isProjectCheckout` is re-checked at the
    removal itself, so a run that recorded the user's own checkout as its
    workspace can never cost them it.
  - A refusal is explained by asking the checkout what state it is in
    (`WorkingTreeChanges`), not by matching git's message: that message is
    localized and free to change between versions, while the working-tree
    question is the same one `git worktree remove` decides on. When the answer
    does not explain the refusal — the checkout is clean, or the question
    itself failed — git broke rather than refused, and its own words are
    reported unchanged rather than replaced by a guess.
  - `App.ProjectDeletionPreview` (`app_project_delete_workflow.go`, LocalOnly)
    describes that cleanup before it runs: the run count, the runs still in
    flight, the automation count, and one row per checkout with its path, its
    branch, its uncommitted-file count, and whether it will be retained. It
    mutates nothing. It shares the *target collection* with the D23 discard
    (`workflowTreeLoss`) — which checkouts a run tree owns is one question with
    one answer — but not the row type: a discard row reports unmerged commits
    as a loss, and under cleanup those commits are not lost, so reporting them
    would be actively misleading.
  - The sidebar previews before it offers anything. Nothing to say keeps the
    one-line confirm; a project that owns runs or automations opens
    `ProjectDeleteDialog.svelte`, which says what is deleted, that the branches
    are kept, and which checkouts will be left behind. A failing preview stops
    the flow rather than falling through to a confirm that would describe a
    project with runs as if it had none. Retained checkouts come back as a
    separate warning toast after the deletion, not folded into the success
    line.

  **Rework note (2026-07-25).** The first implementation of D25 shipped as
  *refuse then cascade*: `DeleteProject` refused a project that owned workflow
  work with a typed error, a second `DeleteProjectDiscardingWorkflowWork`
  method (LocalOnly) consented to it, and consenting ran the full D23 discard —
  forced worktree removal **and branch deletion** — across every run tree the
  project owned, behind a loss dialog listing unmerged commits. That was
  reworked the same day, on the user's ruling, to the cleanup above.

  The reason is a boundary, not a preference. Deleting a project means "remove
  this from Agent Overflow." Git is a system the user owns independently, with
  its own tools for throwing work away; a sidebar housekeeping action has no
  business rewriting their repository as a side effect. The codebase already
  drew the line — see `discardWorkflowTree`'s header
  (`app_workflow_discard_apply.go`): cleanup frees a checkout and leaves the
  commits reachable through the branch, while discard is the human saying the
  work itself is not wanted. Project deletion is cleanup. **D23's per-run
  discard remains the only flow in the app that deletes a branch**, which is
  what makes its preview worth reading.

  Everything the consent machinery existed for went with it: because nothing
  the deletion does is unrecoverable, there is nothing to gate. The typed
  refusal, the second method, and the LocalOnly row that classified it are
  deleted; the preview stays LocalOnly because it still reads local checkouts
  and their uncommitted paths, and `DeleteProject` stays wire-reachable as it
  always was. The rule is pinned two ways in
  `app_project_delete_workflow_test.go`: the branch set of the fixture
  repository is asserted unchanged across a deletion that removes checkouts,
  and `TestProjectDeletionSourceCallsNoBranchDeletion` parses the three files
  that own the flow and fails on any call to `DeleteBranch` or
  `RemoveWorktreeForce` — including in the lines no fixture reaches.

  **Implementation note — the ordering is load-bearing (invariant 35).** The
  cleanup runs *before* `DeleteProject` acquires a single thread lock.
  Cancelling a live run walks engine teardown → `Runner.Stop` →
  `App.InterruptTurn`, which takes `a.threadLocks().Lock(threadID)` on the
  run's phase thread — one of the locks `DeleteProject` holds across every
  thread in the project. Cleaning up under those locks deadlocks outright; the
  regression that pins it is
  `TestDeleteProjectCancelsLiveWorkflowRunBeforeTakingThreadLocks`
  (`app_project_delete_live_run_test.go`), which fails on a timeout rather than
  hanging the suite. Because the cleanup runs unlocked, the locked section
  re-reads the project's run and automation ids and refuses with the same retry
  shape as the thread-set guard if a cron fire changed them underneath it. The
  cleanup also stops the cancelled runs' phase and unit sessions, which settles
  their turn rows synchronously — otherwise a CLI that acks an interrupt and
  never emits a result would block the deletion forever.

  **Implementation note — a project whose checkout is gone stays deletable.**
  Every git question the preview and the cleanup ask goes through
  `readProjectWorktrees` (`app_workflow_discard.go`), which returns an absent
  registry rather than an error when the project directory is no longer on
  disk. There is no repository left to ask and nothing git can be asked to
  remove; the checkouts still sitting on disk are reported as retained with
  that as the reason, and the deletion proceeds. Any other git or stat failure
  still propagates. Making it an error instead would leave the project
  undeletable, permanently and with no other path out
  (`TestDeleteProjectWithAMissingCheckoutStillCleansUp`).

- **D26. A question is one string; UI-SPEC §8's `1`–`9` stay unbound.**
  The control envelope's `question` is a single nullable string
  (`internal/workflow/def/envelope.go` — `"question"` in the generated schema,
  and post-validation requires a non-empty string exactly when
  `status: question`). There is no suggested-answers array on the wire, so
  "pick + send the nth suggested answer" has nothing to enumerate; the shipped
  overlay accordingly registers `workflows.*` commands for toggle/escape/back/
  sweep/action/enter and no digit bindings
  (`frontend/src/lib/stores/workflowCommands.svelte.ts`). The row stays in the
  UI spec as the reserved shape it would take *if* a future envelope revision
  adds choices, and it is deliberately not implemented against a synthesized
  client-side list — inventing options the model did not offer would put words
  in the run's mouth. **Ratified: spec row is aspirational, code is correct.**

- **D27. A wake lost to graceful quit is accepted.** `Shutdown`
  (`app_shutdown.go`) flips `shuttingDown` before step 1a pauses every active
  run, so the pause-triggered wake reaches `registerQueueItem` /
  `sendMessageWithOptions` after the gate and gets `ErrShuttingDown`.
  `reportWakeFailure` (`app_workflow_wake.go`) logs it and emits
  `workflow:error` — into a UI that is closing. Nothing is silently swallowed,
  and nothing durable is lost: the run is parked `needs-human(paused)` in
  SQLite by the same teardown, so at next boot it is in the overlay with the
  needs-attention badge and resumes on the same provider thread (invariant 31).
  The alternative — pausing runs *before* closing the RPC surface so the wake
  lands — would leave a window where the UI can start new work during
  shutdown, which is a worse trade than losing a message that is redundant
  with durable state. **Ratified as accepted behaviour, not a bug.**

- **D28. Provider-schema drift is caught by a cadenced real-provider gate,
  not by CI.** `make provider-smoke` (Makefile) drives one trivial workflow
  through the real `claude` and `codex` binaries and asserts schema
  acceptance, envelope round-trip, and the §9 worktree/branch rules; the
  `providersmoke` build tag keeps it out of `make go-test` and `make verify`,
  which stay hermetic and token-free. **Cadence: before a release, and after
  upgrading either provider CLI.** That is the enforcement mechanism for
  D2a — the mocked suites accept any structured-output schema by
  construction, and `internal/providerschema` encodes rules observed from CLI
  rejections rather than derived from a published contract, so only a real
  run can tell us a rule went stale. The cadence is stated in the Makefile
  comment next to the target so it is discoverable from the thing it governs.
  **Ratified: cadence is the gate; no CI automation of a token-spending
  test.**

## Fan-out ceiling (user ruling, 2026-07-25)

- **D29. Fan-out width has an absolute per-project ceiling, and it refuses
  rather than truncates.** The project profile gains `max_fan_out_width`
  (`internal/workflow/profile/types.go`), the maximum number of units one
  fan-out phase attempt may expand to. Before this there was no bound of any
  kind: the only width check was the dry-run's informational capacity *report*
  (`fanOutWidthReports`), which explicitly skipped dynamic fan-outs because "a
  dynamic `over:` width is a runtime fact". A workflow fanning out over a
  500-element array therefore validated clean and started 500 units against
  subscriptions that cannot pay for them.

  **It is a project setting, not a workflow one.** A workflow that could raise
  its own ceiling would not be a ceiling — the number is a fact about what
  *this* project's provider subscriptions can absorb, not about what the
  portable graph wants, which is the same reason capacities live in the profile
  (§6). There is no workflow-level override and none is planned.

  **It refuses; it never truncates.** Running the first N of a wider expansion
  would hand the join a set nobody chose and record the rest nowhere. A refusal
  is loud, recoverable (raise the ceiling, or narrow the data, then rerun), and
  cannot quietly ship a partial campaign as if it were the whole one.

  **Both shapes are checked before anything starts.** A static `fan_out:` list
  over the ceiling is a blocking dry-run **Finding** (`fan-out.max-width`,
  `internal/workflow/def/validate_fanout.go`), naming the phase, the width, the
  maximum, and the profile key — so `app_workflow.go`'s existing "findings
  refuse a start" gate stops it at the call. The capacity **Report** stays a
  Report and both survive: inside the ceiling but over capacity is pacing, over
  the ceiling is a refusal, and confusing them would make one of the two
  sentences useless. A dynamic `over:` width only exists once the attempt's
  variables resolve, so the engine checks it in `expandFanOut`
  (`internal/workflow/engine/units.go`) — the one seam every unit passes
  through — between `def.ExpandUnits` and `CreateWorkItemUnits`, so a refusal
  leaves no unit row, no sub-worktree, and no provider session. The engine
  check covers static lists too, deliberately: a frozen snapshot is decoded and
  never re-validated, so a run whose definition predates this rule, or whose
  project lowered its ceiling mid-flight, reaches expansion with no finding
  behind it.

  **The park reason is `wiring-error`, reusing an existing one.** Its immediate
  neighbour in the same function — an `over:` variable that is missing or not
  an array at runtime — is already `wiring-error`, documented as "the frozen
  definition and the live context failing to produce runnable work", which is
  exactly what a width refusal is once the live project profile is part of the
  context. `setup-failed` means provisioning (worktree, hooks, secrets, a
  process that would not start) and nothing was provisioned. Adding a reason
  would have cost a `work_items.reason` migration, a frontend signal row, and
  an `ao` mapping to say something an existing reason already says honestly.
  A profile the engine cannot read at expansion parks `setup-failed`, matching
  what a failed resource acquisition does — never an unbounded start.

  **Default 32 when unset; minimum 1; no unlimited setting.** 32 is comfortably
  above any hand-authored `fan_out:` list (the widest in the shipped starters
  and the spec's examples is single digits) and above the realistic dynamic
  case of one unit per section of a plan, while still being a real stop: 32
  units is 32 sub-worktrees, 32 branches, and 32 sessions' worth of
  subscription spend for one phase. Past it a width is almost always a query
  that did not filter. The field is a pointer so "absent" is distinguishable
  from "authored zero": absent resolves to the default through the single
  `def.EffectiveMaxFanOutWidth`, and an authored `0` or negative is a profile
  finding rather than a silently ignored line — a zero ceiling would forbid
  every fan-out, which nobody means, and treating it as "unlimited" would make
  the one setting that exists to bound spend the one that removes the bound.
  A dry-run with **no profile resolved** gets the default rather than a skip:
  the run-start path loads `profile.Default()` for a project with no
  `profile.yaml` and enforces the same number, so skipping would let a
  definition validate clean offline and be refused at its first expansion.
  The ceiling is read live at each expansion (§6), never frozen into the run
  snapshot, so lowering it takes effect on the next attempt of a run already
  under way.

## CLI distribution (user ruling, 2026-07-29)

- **D30. The CLI is the app binary, reached by verb; there is no `ao`
  binary and no `ao` name.** `cmd/ao` was a second `package main` that
  nothing shipped, nothing installed, and no `PATH` ever contained, so
  every `ao run start …` an agent typed answered "command not found" —
  the whole D15 surface existed and was unreachable. It is deleted.
  `main.go` now matches `os.Args[1]` against `aocli.Commands()` and hands
  the argv to `aocli.Run`: `agent-overflow run start <id>`,
  `agent-overflow workflow list`. The verb set is exported from the
  package's own dispatch table rather than restated in `main`, because a
  hand-kept second copy is exactly how a verb becomes unreachable again.

  **Sessions find it on PATH, under a canonical-name symlink.** Boot
  writes `<configDir>/bin/agent-overflow` pointing at `os.Executable()`
  and `sessionProcessEnv` prepends that directory to every provider
  session's environment (`provider.BuildEnvironment` already treats a
  `PATH` override as additive, so the session keeps its own PATH behind
  ours). The symlink exists because the running executable's *name* is
  not stable — `bin/agent-overflow` in a dev build, a temp binary under
  `wails3 dev`, `Agent Overflow.app/Contents/MacOS/Agent Overflow` in a
  bundle — while the name an agent types has to be exactly one thing.
  The directory holds nothing else, so putting it first on PATH exposes
  this command and no other. Replacement is symlink-to-temp + rename, so
  a session resolving the name during a restart sees the old target or
  the new one, never a gap. Non-Windows only: the Windows binary is a
  launcher for the WSL backend and spawns no provider children, so the
  sessions that need the command are published by the Linux backend's
  own boot. A failure to publish is logged and carried, never fatal —
  `cliBinDir` stays empty, PATH is untouched, and the `/workflow`
  composer block says the command may not resolve rather than leaving
  "command not found" as the agent's first news of it.

  **Bare invocation inside a session refuses instead of booting.** With
  `AO_ENDPOINT` set, an argv that names no verb — no arguments, an
  unknown verb, an unknown flag — prints CLI help and exits 2. A second
  GUI process against the same SQLite file is never what an agent meant.
  The decision is a pure function of (args, env) so every branch is
  table-tested: a verb is the CLI in or out of a session; outside a
  session nothing changes and the desktop boots as it always has; and
  inside a session a leading flag the binary actually declares
  (`--harness`, `--connect`, `--data-dir`, `--listen`, `--print-url-fd`,
  `--mock-provider`) still boots, because `make e2e` run from an agent
  session inherits `AO_ENDPOINT` and must stay runnable. "Declared" is
  asked of the same `flag.FlagSet` `parseFlags` uses, not of a list
  beside it.

  **The AO_* variable names do not change.** They are the app's contract
  with its own reader (`internal/aocli/session.go`), never typed by a
  human, and renaming them would churn the injection site, the CLI, the
  harness surface, and every spec for no reader's benefit. Only
  user-facing text moved from `ao` to `agent-overflow`.

## Composer commands (user ruling, 2026-07-29)

- **D31. `/workflow` is a composer slash command that expands at send
  time on the backend; the composer only ever holds the word.** The
  palette entry that pasted the context block into the draft
  (`workflow.composerContext` / `insertWorkflowComposerContext`) is
  deleted, along with the `WorkflowComposerContext` binding it was the
  only caller of. A slash command that answers by dumping a wall of text
  into the composer is not what a slash command is: it is undiscoverable
  (nobody types `/workflow` into a command palette), it destroys the
  draft's readability, and it makes the user carry the block through
  every edit and every re-read of their own message.

  **Typing.** `/` at the start of any WORD in the draft opens a filtering
  completion menu of registered commands, reusing the `@`-mention
  completion machinery (the `Popover` primitive, the `popoverNav`
  reducer, the same insert-through-`execCommand` path so completions stay
  in the native undo stack) — and the same word-boundary rule that
  machinery already uses. Arrow / Enter / Tab / click select; Escape
  dismisses; typing past every match closes it and leaves the text alone.
  A `/` INSIDE a word (`src/lib`, `/tmp/scratch`) is a path, a fraction,
  or prose, and is never hijacked. Selecting completes the WORD in place —
  the draft reads `… /workflow ` and nothing more.

  **Colour is the entire visual vocabulary.** No token, no chip, no
  pill, nothing removable-as-a-unit. Every word matching a registered
  command renders in `--accent`, in the composer and in the sent message
  in chat history. The composer's paint is an invisible mirror of the
  draft — same font, same width, same wrapping, every character
  transparent — whose only ink is the command words, each drawn over an
  opaque `bg-card` background. The TEXTAREA is never made transparent:
  that hides the IME preedit string and the selection highlight, which is
  not a trade a message composer may make. Layout, not measurement, is
  what puts each word in its place, so a word that wraps, sits on the
  fifth line, or moves because the line above grew is still painted
  correctly. The overlay stands down during IME composition and while a
  selection is live, because in both states it would be showing something
  that is not true.

  **Amended 2026-07-29 (user ruling): a command counts at ANY word
  position, not only the first.** The first-word rule shipped as written
  above and was rejected on contact — "it only works if its the first
  word. Thats dumb, not what i intended." A person writing "before you
  start, /workflow" means it, and a rule that silently declines to expand
  because the invocation arrived mid-sentence teaches nothing except
  distrust. The rule is now: a word is a maximal run of non-whitespace, a
  command word is `/` + `[a-z][a-z0-9-]*`, and the first such word the
  registry claims is the command the message invokes — an unregistered
  shape (`/tmp`) is skipped rather than ending the search.

  **The accepted consequence** (explicit, by the user) is that a prose
  mention of `/workflow` genuinely invokes it. There is no heuristic that
  separates "mentioning the command" from "using the command", and one
  that guessed would be wrong silently. The colour is the signal: the
  word is accented from the moment it matches, so the invocation is
  visible before Enter and remains visible in the transcript afterwards.

  **A command named more than once expands ONCE, and colours
  everywhere.** The block is context; the same context twice is only
  cost, and which occurrence "won" is unanswerable anyway because the
  block is appended after the whole typed text either way. One expansion,
  one `meta.command` marker — and every occurrence accented, because
  every one of them is live.

  **Expansion is a send-path concern, and the system prompt is not
  touched.** Putting the block in the session's system prompt was
  rejected outright: it would invalidate provider prompt caching for
  every turn of the thread to serve one message that asked for context.
  Instead `sendMessageWithOptions`, `steerMessageWithOptions`, and the
  flush-queue dispatch all resolve through one place
  (`resolveUserMessageEnvelope` → `expandComposerCommand`), which returns
  BOTH the content to persist and the content to send. The provider gets
  the typed text, a blank line, and the block; SQLite gets the typed text
  and a `command` marker in the user row's meta. A queued message expands
  at DISPATCH, not at enqueue, so the block names the runs that are live
  when it actually reaches the model.

  **History colours from the marker, not from a match.** A row says a
  command expanded because the send that wrote it expanded one. A command
  later removed from the registry keeps its colour on the rows it really
  ran on, and an old row whose text merely looks like a command never
  gains one. WHERE the colour goes is a fresh parse of the stored text
  against the marked name, so the transcript accents the same words the
  composer did.

  **Failure is loud and the expansion is never silently skipped.** A
  resolver error aborts the send before the user row is persisted, so the
  composer restores the draft and shows the error. A block that resolves
  EMPTY still sends and still marks the meta: the message was an
  invocation, and the row must say so even when the project had nothing
  to add. An unknown `/word` is ordinary text, not an error.

  **App-authored messages are exempt, structurally.** Expansion is an
  explicit opt-in (`sendMessageOptions.ExpandComposerCommands`) set only
  where composer-typed text enters the app: the bound SendMessage /
  SendMessageWithOptions / SteerMessageWithOptions wire methods and the
  flush-queue dispatch. A workflow wake, a seeded triage turn, a
  discussion-drive prompt, and a schema-driven phase send never set it,
  and a future internal caller that forgets the flag gets the safe
  behaviour — byte-for-byte delivery — not an accidental expansion.
  Expanding a `/…` opener inside a prompt the app wrote would be the app
  rewriting itself.

  **Two registries, one authority.** The Go table
  (`app_composer_commands.go`) decides what expands; the frontend list
  (`components/composer/slashCommands.ts`) decides what the menu offers
  and what gets coloured. They are parallel by hand rather than
  generated — one entry each today — and the backend is authoritative: a
  word the frontend colours but the table does not know expands to
  nothing and is marked as nothing.

## Workflow surfaces do not spawn threads (user ruling, 2026-07-29)

- **D32. Every affordance that started a NEW chat thread from a workflow
  surface is deleted; opening a thread the run already has stays.** Four
  buttons go, with everything that existed only to serve them:

  1. **"Continue with agent" / "Take over"** — the run-level action on
     the gate, question, failed, blocked, paused, taken-over and done
     rows. It never took a run over: it created a second,
     `workflow-triage`-mode thread seeded with the run record and
     dropped the human into it. The bound method
     (`WorkflowOpenTriageThread`) is unexported;
     `takeOverWorkflowRun` and the `take-over` action id are gone.
  2. **"Open in thread"** — the D17 seed-and-bind affordance on an
     unbound done run. `WorkflowOpenInThread` and
     `workflowOriginThreadOptions` are deleted, and with them the
     `bound` / `isChild` inputs to the action-row table, which existed
     only to decide whether to offer it.
  3. **The triage agent** — the per-project conversational shell behind
     the home header's `Triage` control. `WorkflowOpenTriageAgent`, its
     framing prompt, and `store.FindWorkflowTriageAgentThread` (the
     unlinked-singleton query that distinguished the shell from item
     hand-off threads) are deleted.
  4. **The workflow studio** — `+ New workflow`, the definition row's
     `Edit`, and the empty state's `+ New workflow`. The whole
     `app_workflow_studio.go` RPC is deleted.

  **Why.** These were the surface offering to *start a conversation for
  you*, and that is not how any of this work actually gets done. A run
  worth continuing already has the thread to continue it in — the phase
  thread it ran in, or the origin thread the CLI bound it to. A button
  that opens a fresh, pre-seeded conversation transfers context into a
  place with no context, which is the opposite of the problem it was
  built for. Authoring a workflow is file work (`agent-overflow workflow
  new` + an editor, or an agent the human points at the files), not a
  thread the overlay conjures.

  **What stays, and why the lines fall where they do.**

  - **"Open phase thread"** and every openable phase/unit/attempt row in
    the run tree stay. They open a thread that already exists; that was
    never the objection.
  - **Unit take-over** (`Take over unit`, `t` on the unit-failed row)
    stays whole — `WorkflowTakeOverUnit` runs the engine edge and then
    opens the thread the unit was ALREADY running in. It is the only
    `t` binding left in §8.
  - **The take-over FSM is untouched.** `Engine.TakeOver` /
    `CompleteTakeover` and the runner's detach/schema-swap bookkeeping
    are driven by *sending into a live phase thread* (`app_send.go`
    `prepareWorkflowTakeoverSend`), not by the deleted button. The
    `needs-human(taken-over)` row and its `Finish takeover` action are
    therefore still reachable and still correct.
  - **The wake loop is untouched.** A run bound by `agent-overflow run
    start` (or by `WorkflowBindThread` against an existing thread) still
    injects its digest into that thread on every resting transition.
    Binding never created a thread; only the deleted button did.
  - **`workflowOpenTriageThread` survives unexported** because
    `WorkflowSendPRReviewCommentsToThread` and `WorkflowDiscussPR` (§4.7)
    ride the run's ONE linked thread and this is what makes it one. The
    helper served a removed button and a kept path; the button's entry
    point went, the helper did not.
  - **`threads.mode` keeps `workflow-studio`.** Nothing mints one any
    more, but shipped databases hold them and the hidden-mode exclusion
    must keep hiding them. The mode is data compatibility, not a live
    affordance.

  **A better primitive may replace this later.** The removal is clean —
  no dead code, no commented-out buttons, no orphaned bindings — so that
  whatever replaces it starts from the behaviour that is actually wanted
  rather than from this one's shape.

## Repairing a fan-out is one action, not N (user ruling, 2026-07-29)

- **D33. A parked fan-out gains one action that retries EVERY failed unit
  at once, alongside the per-unit retry that stays.** `Engine.RetryFailedUnits`
  → `App.WorkflowRetryFailedUnits(itemID, note)` → the `Retry all failed
  units` button (`u`, §8) and `agent-overflow run retry-failed-units
  <run-id> [--note]`.

  **Why.** The failure this repairs almost never arrives one unit at a
  time. A wide fan-out hits a provider usage limit and most of its units
  fail against the same wall inside a minute of each other. The human
  clears that cause exactly once — waits for the reset, or switches
  account — and then has one decision to make about the whole attempt.
  Making them press `a` once per unit turns a single decision into N
  identical clicks, each of which reloads the detail and re-picks the
  next failed unit. The single-unit retry stays because the other shape
  is real too: one unit failing on its own merits, which is a decision
  about that unit.

  **It is one engine command, not N submitted retries.** The command
  loop serializes commands but not the gaps between them, so N
  `RetryUnit` calls could interleave with a concurrent drop or single
  retry and reach the second half of the set against an attempt the
  first half had already returned to `running`. The retry-all reads the
  failed set, reopens every member, and resumes the attempt inside one
  turn of the loop, so no other command ever observes a half-repaired
  fan-out. The set is collected before the first store write, which is
  what makes "no unit is failed" a refusal that changed nothing rather
  than a repair that happened to write none.

  **No new admission path, and therefore no burst.** It resumes through
  the same `resumeRepairedFanOut` the single retry uses: the attempt row
  is reopened (finished units keep their results), the item returns to
  `running`, and each repaired unit is then admitted one at a time
  through `acquireUnitResources` — queuing in the same FIFO a held phase
  start uses when the project is at capacity. Repairing twenty units
  against a provider bound of two starts two and holds eighteen. Anything
  else would make the action that recovers FROM a usage limit the one
  action that ignores the bound modelling it.

  **Units under human steering are not repaired.** `taken-over` is the
  human driving; the retry-all leaves those units and the attempt stays
  parked on them — exactly what retrying each failed unit by hand would
  have produced. The action's contract is "every failed unit", not
  "return this run to running".

  **Agent-callable under the grant the single retry already carries**
  (`start-run`, `ScopedTokenMethods`). The session babysitting a campaign
  run is the one that notices the reset — it is polling `agent-overflow
  run status` while the human is asleep — so the verb has to exist where
  that session is. Repairing every failed unit at once is not more
  authority than repairing them one at a time: same edge, same admission,
  same rows, one command.

  **CLI shape: a sibling verb, not a flag.** `retry-failed-units` differs
  from `retry-unit` in arity, not in options. A `--all` flag on
  `retry-unit` would make its second positional conditionally required —
  the shape where a mistyped invocation silently repairs the wrong thing
  — so the two are separate verbs and each refuses the other's arity.

## Campaign waves are content, not an engine (user ruling, 2026-07-29)

- **D34. The multi-wave campaign ships as a starter, with split-context
  adversarial review as its default review shape, and chains through
  automations rather than through a loop primitive.** `port-campaign`
  (`internal/workflow/starters/content/port-campaign/`) is one campaign
  *wave* — survey, plan, fan-out implement, adversarial review, fix,
  verify — plus an authoring guide
  (`docs/architecture/workflow-campaigns.md`) for the patterns and the
  wiring around it.

  **It is a starter, never a built-in tier.** Everything the campaign
  needs already exists as authoring surface: dynamic fan-out over a typed
  array output, per-unit providers, tool joins, ordered gates, bounded
  loops, and §11 automations. A campaign "mode" in the engine would be a
  second definition of what a run is, and D1's rule — disk is the source
  of truth, the engine loads no hidden definitions — is what keeps
  `agent-overflow workflow validate` an honest answer. So the campaign is
  a file the user scaffolds and edits, and the only code change is one
  more name in `starters.List()`.

  **Split context is structural, not a prompt convention.** The
  implement phase's join is a `command:` (the profile-bound merge), not
  an agent, so the only things crossing from implementation to review are
  the merged tree and the commit it is diffed from — no implementer
  narrative, no unit summary, nothing a reviewer could grade instead of
  the code. Reviewers are prompted to *refute*, three lenses rather than
  three copies, and the join treats every claim as a lead it has to
  reproduce before it becomes a finding. Approval-shaped prompts get
  approval; identical reviewers correlate; an unadjudicated claim spends
  the next phase on something that was never wrong.

  **Waves chain through the scheduler, and the shipped recipe is cron +
  skip-if-running, not an internal-event chain.** The loop is meant to
  live in a trigger plus its run-if (Core Principle 1: no second
  orchestration engine), and that part stands. What does *not* work is
  the obvious form of it: an event-triggered automation that starts the
  same workflow it triggers on is refused as `self-chain`
  (`scheduler/fire.go`) — deliberately, because it would chain forever.
  A pair of alternating automations is legal and documented, but it has
  two sharp edges: a seed run started outside the pair matches *both*
  triggers and starts two concurrent waves, and the event feed races
  auto-disposition off the same terminal transition, so the next wave can
  cut its worktree before the previous one's merge lands. A cron trigger
  has neither problem — skip-if-running is per-automation and already
  refuses a fire while the previous run is `running` **or**
  `needs-human`, so the campaign advances unattended, never doubles up,
  and halts on any park until a human clears it. The event pair stays in
  the guide as the low-latency variant with its caveats stated.

  **There is also no surface that could author the event pair today.**
  `agent-overflow schedule` creates a cron trigger with seeds and
  nothing else; the overlay lists automations, toggles them, and offers
  Run now, but has no create/edit form, so event triggers and run-if
  conditions have no user-reachable authoring path even though
  `WorkflowCreateAutomation` accepts both. Shipping the campaign against
  a shape nobody can type would have made the guide fiction. The gap is
  noted rather than closed here — an automation editor is its own scope
  conversation, not a rider on a starter.

  **A run-if cannot ask "is there work left".** Conditions are evaluated
  against the seed map alone — stored seeds plus the reserved `trigger`
  and `job-notes` — and cannot read the finished run's outputs. So the
  question is answered structurally instead: a wave with work remaining
  ends `done` and the next fire proceeds; a wave with nothing left parks
  `campaign-complete`, and a parked run blocks every later fire. The
  run-if keeps a real job — an arming switch that stops the campaign
  without deleting the automation.

  **`disposition: auto-merge` is a documented prerequisite, not a
  default we added.** Each run cuts its worktree from the base branch, so
  without a landing step every wave replans the same work. Making the
  starter assume it would have been a workflow reaching into project
  policy, which is exactly the split §8 draws; the guide states it, and
  the wave's `cleanup: auto` discards a worktree only after a disposition
  has landed.

  **SUPERSEDED IN PART by D37 (2026-07-29).** Everything above about
  *what a starter is* stands: the campaign is content, never an engine
  mode, and split-context adversarial review is still its default review
  shape. What does not stand is the **chaining**. Every paragraph above
  describing waves chaining through the scheduler — the cron +
  skip-if-running recipe, the alternating-automation pair, the
  `self-chain` refusal, what a run-if can and cannot ask, and the
  `disposition: auto-merge` prerequisite that existed only because each
  fire cut a fresh worktree — is superseded. Waves now chain through a
  self-call, which is the primitive §3a gives for exactly this and which
  did not exist in usable form when D34 was written. The user's verdict
  on the automation-chained starter was "frankenstein shit", and the
  reasoning holds up: it needed a scheduler row that could not ship in
  the file, an authoring surface that does not exist, a landing policy in
  the project profile, and a worktree per wave — four moving parts
  outside the definition to express one loop. The scheduler facts
  recorded above remain true of automations; they are simply no longer
  how a campaign iterates.

## A fan-out unit can be a call (2026-07-29)

- **D35. A fan-out unit binds to exactly one of `prompt:`, `command:`,
  or `call:`, and a call-bound unit runs its child in that unit's own
  sub-worktree.** §3a's workspace rule already said a call executes in
  "the unit's sub-worktree when called from inside a fan-out", but the
  unit template had no `call:` to reach that sentence with. This closes
  the gap: `def.Unit` gains `call` / `args` / `max_depth`, the engine
  spawns a child run per call unit, and the child's declared `outputs:`
  become the unit's envelope.

  **It exists because a campaign's unit of work is a sub-workflow, not
  a turn.** The shape the campaign wants is "each work item gets its own
  implement → review → fix loop, on its own branch, and the join merges
  what survives". Without a unit call, the only way to express that was
  one enormous unit prompt doing all four steps in a single session —
  the exact opposite of the split-context reason D34 gives for making
  review a separate phase in the first place.

  **Three exclusive bindings, not a `driver:` with a third value.**
  `Unit.EffectiveDriver()` now answers `(Driver, bool)` rather than a
  bare driver, so no caller can read a call unit as an agent unit by
  forgetting the case; a call unit answers `EffectiveShape() ==
  ShapeCall`, reusing the phase-level shape rather than inventing a
  parallel discriminator. Everything a call unit could otherwise
  declare — provider/model/prompt, command, access, and **outputs** — is
  a validation finding, on the same reasoning `validateCall` refuses
  them on a call phase: the child workflow's phases carry all of it.
  Refusing `outputs:` outright is stronger than "a mismatch with the
  child is a finding", and deliberately so — a unit's outputs *are* the
  child's declared outputs, so any declaration is either a duplicate or
  a lie, and there is no third case worth admitting.

  **A join may not be a call.** Its envelope IS the phase's, and every
  phase-level continuation (`Answer`, `CompleteTakeover`, a resume in
  place) is a continuation of the join's own session. A call join would
  leave those actions with nothing to continue, so it is refused with a
  message pointing at the unit.

  **Unit call edges are call-graph edges everywhere, not only in the
  engine.** `CallTargets`, `PropagatedWorkspaceNeed`, the dry-run's
  cycle detection, the child-validity memo, and the argument checks all
  traverse unit edges through the same `validateCallEdge` a phase edge
  uses. A unit's `args:` resolve against `ResolveUnitDeclarations` — the
  phase's inputs plus the `as:` element binding — which is exactly what
  a unit *prompt* could reference, so the two cannot disagree about what
  is in scope. A cycle closed by a unit edge needs `max_depth` on that
  unit, counted per (workflow, phase, unit) so siblings never spend each
  other's budget.

  **The unit's sub-worktree is the app's answer, not a new engine
  concept.** The engine stamps *no* workspace on a unit-call child
  (`callInvocation.inheritWorkspace` is false only here) and the app
  resolves it through `ParentUnitID` linkage, calling the same
  `provisionUnitWorktree` a writing agent unit goes through, keyed on
  the unit row's try number. So the checkout is adopted-or-cut once,
  registered on the unit row before anything runs in it, and the join,
  the discard preview, and `retireUnitWorktrees` all find it without
  knowing a child run was involved.

  **Outcome mapping.** A `done` child settles the unit `done` with the
  child's declared outputs; a `failed` **or** `cancelled` child settles
  it `failed` with a note naming the run (there is no cancelled unit
  status, and to a fan-out both are "this unit produced no result"); a
  `needs-human` child is not terminal, so the unit keeps resting. A
  `done` child whose declared outputs cannot be read fails that unit
  rather than the attempt — the siblings' work is durable. From there
  the ordinary unit-failure policy applies: stop new launches, let
  in-flight units finish, park `needs-human(unit-failed)`.

  **Recovery.** A call unit's `running` row is a live relationship, not
  a dead runner, so the crash sweep must not fail it. `recoverFanOutCalls`
  fails the runner-backed rows, restores the fan, and re-links each
  resting call unit to its newest child (re-invoking in place when the
  crash landed between the unit row and the child, which is why the row
  is written first). Pause keeps those children via
  `retainCallChildren`, and resume re-links them rather than starting
  replacements. Cancel and discard cascade through them by reading the
  children from the *store*, because the rebuild path tears an attempt
  down with no in-memory fan at all — the one case where a stranded
  grandchild would otherwise survive a restart.

  **Child definitions were already re-resolved per invocation; the
  campaign depends on it, so it is now covered.** `startCall` /
  `startUnitCall` both call `DefinitionSource.ResolveCall`, which
  resolves from disk and re-inlines prompt bodies every time
  (`app_workflow.go` `resolve` → `def.InlinePrompts`). The caller's own
  remaining phases keep their frozen snapshot; the next wave gets the
  file as it is now. Nothing changed here — a regression test was added
  because a cache introduced later would silently break the workflow the
  campaign is built around.

  **`MaxCallDepth` rises from 32 to 256.** `def` never capped
  `max_depth` (only `< 1` is refused, and the authoring schema states no
  maximum), so a campaign declaring 120 validates — but the engine's
  absolute ceiling would have refused wave 33. The ceiling exists to
  stop recursion that live resolution introduced after a dry-run, and 32
  was sized for accidental recursion, not for a deliberately long chain.
  The cost of depth is the O(depth) parent walks (call chain, budget
  root, workspace root, pause/cancel subtree), each an indexed point
  read, so hundreds is bounded by arithmetic; runaway recursion still
  dies here, hundreds of runs before anything else notices.

## Stopping a campaign, and hearing about a wave that stopped itself (2026-07-29)

- **D36. A run tree gains a standing request to stop at its next CALL
  boundary, parking `needs-human(checkpoint)`; and a descendant's park
  carries enough at the root's bound thread to be acted on without a
  second command.** `Engine.SetSoftStop` →
  `App.WorkflowRequestSoftStop(itemID, armed)` → the `Stop after this
  wave` button on a running root's action row and `agent-overflow run
  soft-stop <run-id> [--clear]`.

  **Why a third stop.** Pause interrupts the turn in flight; cancel ends
  the run. Neither is what a human wants at 2am watching a campaign that
  has three good waves behind it: they want the wave that is running to
  finish, and the next one not to start. Before this, the only way to get
  that was to watch for the boundary and press Pause inside it — which is
  a race a human loses, and which costs the interrupted turn's work when
  they lose it.

  **The boundary is the call, and only the call.** `startCall` reads the
  request before it resolves a target, evaluates args, or writes a child
  row: the wave that just ran is complete, the next one has not begun, and
  stopping costs nothing. It is deliberately NOT checked at a fan-out
  unit's call edge — a unit call is work *inside* a wave, and stopping
  mid-fan-out would strand the siblings the join is waiting for, which is
  the interruption this feature exists to avoid.

  **The request lives on the root, and the firing boundary consumes it.**
  A tree is stopped as a tree, exactly like pause (§12), so `SetSoftStop`
  refuses a called run and names the run to set it on instead. Every
  descendant's boundary reads the ROOT's row, which is what lets a
  request made against the run a human is watching stop a wave forty deep.
  The boundary that fires clears the flag before it parks — so a resume
  takes the call it skipped instead of re-parking on the same boundary
  forever, and a cleared flag with a failed park is a loud error rather
  than a tree that can never move again.

  **The engine is the only writer.** `SetSoftStop` goes through the
  command loop rather than writing the row from the caller's goroutine,
  because the boundary's read and its clear are on that loop too. Writing
  from outside would open a window where a request lands between a
  boundary's read and its clear and is silently lost. For the same
  reason the boundary re-reads the root row from the store instead of
  trusting the resident copy.

  **`checkpoint` is a new typed reason, not a reuse of `paused`.**
  Migration v44 widens the `work_items.reason` CHECK and adds the
  `soft_stop` column (one rebuild, derived from v43's text). It resumes
  through exactly the `paused` / `interrupted` edge — same continuation,
  same `ResumableReason` set — but it reads differently everywhere a
  human looks, because it is the one park that is not a fault: the run
  did what it was told. Calling it `paused` would have made the benign
  stop indistinguishable from the six that need diagnosing.

  **Arming refuses a run that is not going; clearing never refuses.** A
  request on a parked or finished run has no next boundary to reach and
  would fire at whatever a later rerun did first. Withdrawal stays legal
  in every state, because "I changed my mind" must never be the thing
  that errors. Both directions are idempotent and both travel through the
  one method with a flag, so a caller that can only arm cannot exist.

  **A workflow with no call edge accepts the request and never fires it.**
  That is stated in `agent-overflow run soft-stop --help` and is why the
  UI offers the button only when the run's frozen snapshot actually has a
  call phase (`WorkflowItemDetailView.CallPhaseIDs`): a request that could
  never fire must not be presented as a stop that will happen.

- **D36a. A descendant's park was already announced at the root; what it
  said was not enough to act on.** `surfaceDescendantPark` (spec §5
  amendment, 2026-07-25) has always fired for a park below a running
  root. The wake now also carries the call chain root→park
  (`wake.Descendant.Chain`, elided in the middle past `MaxChainRuns` with
  the elision stating its own size), the parked run's OWN failed units
  rather than the root's, and a closing that says which run to act on.

  **Why the chain.** A campaign is one run per wave, so "a called run 6
  levels down parked" names a run the reader has never seen, in a tree
  they would otherwise have to walk with a second command to understand.
  The chain is cheap — the ancestry walk that resolves the root already
  visits every row — and it is what makes `agent-overflow run
  retry-failed-units <child-run-id>` a command an agent can issue from
  the message alone.

  **Why the descendant's units and not the root's.** The most common
  thing the woken agent is there to do is repair the parked run's failed
  units. Reporting the root's would point the repair verb at the wrong
  run, so the reference label names whose units they are
  (`called run failed unit`) and the root's are not collected at all.

  **The authority to act on them was already correct.** An interactive
  scoped token (the credential a bound thread's session holds) is
  PROJECT-scoped: `app_workflow_cli.go`'s `scopedRun` checks
  `item.ProjectID != scope.ProjectID` and then returns for any non-phase
  scope, so a descendant run is reachable without widening anything. A
  phase token stays limited to the runs it started itself, descendants
  included. A test now pins both.

- **D36b. `store.RetryWorkItemUnit` persisted neither the retry note nor
  the attempt bump.** The engine incremented `unit.attempt` and attached
  the feedback in memory, and the store reset the row to `pending`
  without writing either — so a retried unit that was evicted and
  restored came back on its old try number with no feedback, and the
  repaired unit's prompt lost the note that told it what to do
  differently. The store call now takes both and writes both; the restore
  path reads them back; the test that pinned the old behaviour asserts
  the new one. There is no double-bump: the engine computes the next
  number once and both `RetryWorkItemUnit` and the later
  `StartWorkItemUnit` persist that same value. The four call sites that
  had each open-coded the reopen now share `Engine.reopenUnit`.

## The campaign is a call tree (user ruling, 2026-07-29)

- **D37. The campaign starter is two workflows: a spine that calls itself
  once per wave, and a lane that each implement unit calls once per
  task.** The Wave E starter was one wave chained from outside by an
  automation (D34); the user rejected it — "frankenstein shit" — and
  everything it worked around now exists natively. §3a call phases
  re-resolve their target from disk **per invocation**, definition and
  prompt bodies both. D35 made a fan-out unit able to *be* a whole child
  workflow in its own sub-worktree. D36 gave a tree a stop that fires at
  a call boundary. So the loop is a call edge, the isolation is a unit,
  and the campaign is one root run instead of forty unrelated ones.

  **`port-campaign` — the spine.** `plan` (agent, **Claude**,
  read-only) → `implement` (fan-out over `plan.tasks`, every unit a
  `call: port-one-task`, join is the profile-bound merge command) →
  `resolve-conflicts` (only on a dirty merge) → `verify` (tool check on
  the merged campaign branch) → `integration-fix` (only on red, loops
  back to `verify` bounded at 2) → `next-wave` (`call: port-campaign`,
  `max_depth: 200`). Fable plans because choosing what a campaign does
  next is the judgment call; the work is GPT's.

  **`port-one-task` — the lane.** `implement` (Codex, write) → `review`
  (fan-out: one Codex *fidelity* lens beside one Claude *consequence*
  lens, join adjudicates their claims against the branch) → `fix`
  (Codex, write) → **back to `review`**, bounded at 2. Two models
  because two copies of one model miss the same things twice; the fix
  re-enters review because a fix is a change nobody has looked at yet.
  The lane cuts no worktree of its own and merges nothing: D35 gives it
  the unit's sub-worktree, and the spine's join owns the merge.

  **The exit is the absence of a call, not a park.** The planner emits
  `complete`, and `plan`'s first gate route sends a complete wave `to:
  done`. The run finishes without ever reaching `next-wave`, which
  settles the wave that called it, which settles the one above — the
  whole tree unwinds. Expressing the exit as "do not take the call edge"
  rather than as a park is what makes a finished campaign a *done* tree
  instead of a stack of runs each waiting for a human.

  **`checkpoint-every` is a human gate the planner arms.** A gate
  predicate cannot do arithmetic — the language is comparisons,
  membership, existence, and combinators (§4) — and `args:` are
  references, not expressions, so "wave % N == 0" is not expressible in
  `def`. The wave number is therefore engine-carried (a required input,
  handed forward as `wave-number: plan.next-wave-number` through the
  self-call's args, which the dry-run type-checks) and the *comparison*
  is a planner output: `plan` emits `checkpoint-due`, and `verify`'s
  gate turns it into a `human:` route — approve advances to
  `next-wave`, reject loops back to `plan` with the human's note. A
  `park:` would have been wrong here: a park resumes by re-running the
  phase that parked, so it would re-evaluate the same gate and park
  again forever, and only a `human:` route has an approve edge that
  advances. This is the one place a model computes something the engine
  could have: getting it wrong costs one missed or one extra pause, and
  the alternative — a bound command whose only job is modulo — would
  have added a profile binding to every campaign for it.

  **The loop bound is a literal, and stays one.** `max:` on a loop route
  is validated statically (`gate.loop-max`, `max >= 1`) and cannot read
  a variable, so the lane's review↔fix bound cannot come from a workflow
  input. It ships as `2` with the YAML saying, at that line, that
  raising it is an edit to the number. In a system where the workflow is
  a file the user scaffolds and owns, editing the file *is* the
  flexibility; inventing a counter output so a gate could compare
  against an input would put a model in charge of a bound whose whole
  job is to be a bound.

  **Two starters, not one set with two definitions.** A `Set` is a flat
  directory scaffolded under one `--id`, so a two-definition set would
  need the scaffolder to invent an id for the second one and would
  collide with itself on a second campaign. Shipping them as two names
  keeps `--id` meaning what it says and lets several campaigns share one
  lane. The cost is that the spine's `call: port-one-task` names an id
  the scaffolder cannot rename — so scaffolding the spine alone produces
  one `call.target` finding, printed at scaffold time, naming exactly
  what is missing. `workflow new` **does** now rewrite a definition's
  calls to *itself* (`rewriteSelfCalls`), because a self-call is part of
  the definition being renamed and leaving it stale would point a
  campaign's loop at a workflow the user never created.

  **Disposition is manual and the campaign is one branch** (user
  ruling). Workspace flows down the call stack (§3a/§9), so every wave
  runs in the root's worktree on the root's branch: the planner's
  evidence is the tree every previous wave landed in, no landing step is
  needed between waves, and D34's `disposition: auto-merge` prerequisite
  is dropped rather than restated. The branch is the deliverable and a
  human decides what happens to it.

  **Steering has three speeds, and the guide states which is which.**
  Prompt files and repo context files take effect on the **next wave**
  (calls re-resolve from disk); workflow inputs are fixed for the
  campaign; and stopping is `run soft-stop` (any wave boundary, on
  demand) or `checkpoint-every` (on a schedule). The one that used to be
  live — an automation's job notes — is now a start-time input, which is
  a real loss of a knob and the reason repo context files are prompted
  for explicitly.

## Every parked state names its own verb (2026-07-29)

- **D38. A surface that reports a parked run must name the command that
  repairs it — and say so plainly when no command does.** The park reason
  was already on every surface; the mapping from reason to verb lived
  only in the heads of people who had read the FSM. A cold agent reading
  `needs-human (unit-failed)` knows something is wrong, has four control
  verbs that all sound like stopping and starting, and guesses.

  **The map, once, in three places.** `/workflow`'s composer block
  carries it as a four-line table (`composerRepair`); a wake's closing
  carries the one line that applies to the run it is about
  (`wake.repairSentence`); the phase prompt suffix carries the two the
  phase itself can trigger. `paused|interrupted|checkpoint` →
  `run resume`, state `failed` → `run rerun`, `unit-failed` →
  `run retry-failed-units` (or `run retry-unit <unit-id>`).

  **Naming the absence is half the ruling.** `gate` and `question` have
  no CLI verb on purpose — they are the judgments the system exists to
  route to a human — and every other reason (stuck, stalled,
  agent-error, wiring-error, setup-failed, budget/retries-exhausted,
  check-failed-genuine, taken-over, child-failed, disposition) is
  repaired by fixing the cause the reason already names. Leaving those
  out of the map would read as "there is a verb I have not found yet",
  so the map states that they are not CLI-repairable rather than
  omitting them. A generic "resume" printed for every reason would be
  precisely the wrong guess made confidently.

  **The verb needs its arguments, so the arguments became readable.**
  `run status` now carries the run's `failedUnits` (the second argument
  of `run retry-unit`, resolved on the single-run read only — a list
  would pay one unit query per row) and both `status` and `list` render
  `parent=<run-id>`, so a campaign's flat list shows the tree that
  relates its waves. The wake's command interpolates the id of the run
  being acted on, which for a descendant park is the **descendant's**.
  Run ids stay `untrustedtext`-quoted inside the command: they are still
  model-adjacent data, and a paste-able command is not a reason to stop
  quoting one.

  **`run list` with no rows says so.** Printing nothing reads as a
  command that failed, which sends an agent looking for a broken
  session. `--json` is unchanged: `[]` was never ambiguous.

## The narrative always exists, and the CLI can see the project (2026-07-29)

- **D39. A phase's account of its work is not conditional on its access, an
  offline command inside a session can infer its project, and a directory
  discovery ignores says so.** Three defects a live two-provider read-only
  smoke run surfaced, all of the same shape: the system asked for something,
  did not get it, and said nothing.

  **The narrative is recovered from the final message, never invented.** A
  `read-only` phase maps to a provider runtime mode that denies every file
  write (D22), so the system suffix's "write your narrative to this file" was
  an instruction it could not follow. A completed read-only run left every
  attempt directory empty, the wake pointed at a path nothing had created, and
  the triage seed read "narrative unavailable" for work that had been done
  perfectly well. Two halves fix it. The suffix now varies on access: a writing
  element still gets the path, and a read-only one is asked for the narrative as
  **the message immediately before its envelope** — which is what it was going
  to produce anyway. The runner then writes that message to the file when the
  envelope is accepted and no file exists, prefixed with a line marking it as
  recovered.

  **Marked, last-only, and never overwriting.** The header exists because a
  human must be able to tell a reconstructed account from an authored one; the
  content is the LAST assistant text rather than a concatenation, because the
  account a phase gives before closing out is the one it means; and an existing
  file always wins, because an agent that wrote a file wrote something richer
  than a message. A session that said nothing gets no file — absence stays
  absence, so "the phase produced no account" and "the phase produced an empty
  one" stay distinguishable. The envelope is passed to the recovery so the text
  that IS the envelope can be skipped: Codex emits its structured output as its
  final message, and recovering that JSON would be worse than recovering
  nothing. The test is identity with the envelope as a decoded document, never
  "looks like JSON" — prose that happens to be JSON is still prose.

  **A reference that does not resolve is worse than no reference.** The wake
  built its narrative pointer from the path arithmetic alone. With the recovery
  in place most runs have the file, but a phase that produced neither still
  exists — so the wake (and the descendant's `called run narrative`) now
  includes the pointer only when the file is there. An agent that opens a
  reference and finds nothing has spent a tool call learning the message was
  wrong.

  **AO_PROJECT joins the session contract.** `agent-overflow workflow list` and
  `workflow validate --id` resolve project-scoped definitions from a directory
  named by project **slug**, and the offline half has no app to infer one from:
  without `--project` they silently resolved shared scope only, and a cold agent
  inside a session had no way to learn the slug at all. The slug now ships
  alongside `AO_ENDPOINT` / `AO_TOKEN` / `AO_THREAD_ID` for every session that
  has a project, which is every session carrying a credential. The read
  commands default their scope from it and an explicit `--project` always wins.
  `workflow new` deliberately does not inherit it: `--project` is that command's
  write *destination*, and an env var that silently redirects where files land —
  with no way left to target the shared scope — is a different and worse bug than
  the one being fixed. The `/workflow` block names the slug on its project-scope
  line for the same reason the variable exists: a reader who has only the block
  must still be able to write the flag.

  **A skipped directory is reported, not logged.** Discovery is flat by design —
  a workflow is `<id>.yaml` beside its `<id>-*.md` prompts — so a hand-authored
  `<id>/workflow.yaml` resolved to nothing with no error and no row, which is
  exactly what the smoke run hit. `def.SkippedDirs` returns the directories that
  hold YAML and were skipped; it does not warn, because the engine resolves on
  every run start and a definition directory is not the engine's business to
  narrate. `workflow list` renders one note per directory, and a failed
  `validate --id` carries them inside the not-found error, where the caller is
  already looking. The notes go to stderr in both modes so `--json`'s document
  stays exactly the list.

## The narrative rides in the envelope (2026-08-08)

- **D40. A read-only element's account is an optional `narrative` control
  field, not a separate message.** D39's read-only half was written against
  Claude's `--json-schema`, which constrains only the final structured output,
  and it is unfollowable on Codex: `turn/start outputSchema` is a whole-turn
  Responses-API `text.format` constraint, so EVERY assistant message of a
  schema'd turn is forced into envelope JSON and the element cannot emit prose
  at all. Two things followed from that on Codex — the suffix asked for a
  message the element could not send, and the D39 fallback then recovered the
  envelope JSON blob as the "narrative" for exactly the elements the fallback
  exists for.

  **The envelope gains a fifth control name.** `narrative` is an optional
  top-level string beside `status` / `outputs` / `question` / `reason`, legal on
  **every** status — a done, a question, and a stuck element all did work worth
  an account, and refusing it anywhere would burn the element's single envelope
  retry on the one field that is never a mistake. It is the only control field
  the generated schema requires and post-validation does not: strict mode's only
  optional is required-and-nullable, while a tool command writes its envelope by
  hand and every envelope frozen before this decision omits it. Outputs nest
  under `outputs`, so an author may still declare an output named `narrative`;
  the two never meet, and no reserved-name check was added for a collision the
  structure already prevents.

  **Stripped at one seam, so nothing downstream sees prose.** The app lifts the
  field out where every agent-backed turn already reports — phases, units, joins,
  `Answer` continuations, takeover finalizes — and hands the engine the envelope
  without it, so gate evaluation, a join's `units` results, call synthesis, and
  the persisted attempt envelope are unchanged in shape. The lifted text becomes
  the attempt's narrative file with **no** recovered header: the element
  deliberately put it there, so it is authored exactly as a file it wrote would
  be. An existing file still always wins.

  **The write branch is untouched, and the field is still explained to it.** A
  narrative authored during the work is richer than one summarized into a field
  afterwards, so a `write` element keeps the file instruction as primary and is
  told to null the field. It is told rather than left silent because the schema
  makes every element answer it, and an unexplained required field is one a
  phase guesses at.

  **A command may write one too.** Post-validation is written once against the
  contract for both drivers, so refusing `narrative` in a tool envelope would be
  a second rule set for one contract. It is folded into the same narrative file
  the masked output tail goes to, leading it — the account is the only part of
  that file a human did not have to reconstruct from a process.

  **D39's fallback still exists, and now survives Codex.** An element that
  supplies neither file nor field falls back to the session's final assistant
  text as before. A candidate carrying a top-level `status` is read as an
  envelope rather than as prose, and what is recovered from it is the account it
  holds (`narrative`, else `reason`) — never its raw JSON; one with no account is
  skipped like the envelope echo. That shape test is deliberately weaker than
  D39's document-identity test and applies only to candidates that already
  failed it.

## Agents settle parks, budgets are seedable, units claim capacity (2026-08-09)

A multi-wave campaign run surfaced these as one cluster: its babysitting agent
could not decide a park it had the judgment to decide, could not size a loop
budget per run, could not put a command unit under capacity accounting, and
could not see which attempt a gate had actually consumed. All four were
authority or visibility the system withheld, not capability it lacked.

- **D41. A park is settleable from the CLI, under a grant an author hands out
  deliberately.** D38 ruled that `gate` and `question` have no CLI verb — they
  are the judgments the system exists to route to a human. That ruling assumed
  a human is watching an approval surface; in practice the watcher is an
  interactive agent session babysitting a campaign, there is no approval UI,
  and "surface it, don't answer it" meant every human gate was a dead stop.

  **Two verbs, one new grant.** `run resolve <run-id> --approve|--reject
  [--note]` decides a `needs-human(gate)` park through the same
  `ResolveHumanGate` path the app would use; `run answer <run-id> <text>`
  answers a `needs-human(question)` one. Both are admitted by the new
  `resolve` grant — deliberately separate from `start-run`, because starting
  and stopping work is routine while answering a decision the workflow author
  routed to a human is authority an author must hand out per phase. An
  interactive session holds every listed method implicitly (its every
  invocation passes the provider's own approval UX); a phase session needs
  the grant, and even then may settle only the runs that phase started — the
  same row-level confinement every acting verb has.

  **The map and the wake now say so.** D38's "naming the absence is half the
  ruling" paragraph is superseded for these two reasons: the composer repair
  map and the wake closing name the verbs, tell a phase session it needs the
  grant, and tell the reader to decide only what is theirs to decide. The
  wake also gained the `retries-exhausted` line D38 left out —
  `run resume --phase <id>` re-enters an earlier phase and refills loop
  budgets, a resume in place does not — because that reason DID have verbs
  and printing nothing read as "no verb exists".

  **The verb needs its evidence, so `run status` grew provenance.** Deciding
  between `run resolve`, `run resume --phase`, and `run rerun` requires
  knowing which attempt produced the outputs the gate consumed and what it
  ran with. All of it was already persisted — attempt rows carry status,
  thread, and gate trace; workflow threads are stamped with resolved
  provider/model/effort at creation — so the single-run status read now
  renders one line per phase attempt (phase, attempt, status, provider,
  model, effort, gate decision, exhausted edges), resolved by one joined
  query and never on `run list`, the same bounded-cost rule `failedUnits`
  set.

  **Amendment (same day): `run resolve` settles `human:` routes only — a
  `park:` route is undecidable by construction, and every surface now says
  which is which.** The first agent to hold the verb aimed it at a `park:`
  park and got "persisted decision is park without a human intervention" —
  engine-speak that reads as a corrupt record rather than as the construct
  working. The two route forms rest under the one `gate` reason, but only
  `human:` declares an approve target and a reject loop; resolving it
  completes the parked attempt, which is why its outputs survive into the
  variable context. A `park:` route declares no continuation — there is
  nothing an approve could select, and no completion path exists that would
  carry its outputs forward — so its repair is `run resume` (a fresh attempt
  of the phase), and an author who wants the stop to be decidable writes a
  `human:` route. What changed: the refusal now says exactly that and names
  the repair; the wake's gate closing branches on the persisted decision kind
  (resolved app-side from the gate trace, best-effort — an unreadable trace
  names both verbs rather than guessing); the composer repair map carries
  both gate rows keyed by the `decision=` field `run status` already renders;
  and `run resolve --help` states the boundary. No new authority was added —
  `resume` always accepted a park:-route park (`isHumanGate` is what fences
  the human form) — this is the D38 rule applied to the distinction: a verb
  that cannot act must say so where the reader is already looking.

- **D42. A loop bound is a literal or a variable reference, resolved at
  evaluation time.** `max: 2` froze retry budgets into the definition; a
  campaign wanting a deeper budget for one run had to edit shared YAML. `max:
  inputs.fix-budget` now resolves against the run's variables each time the
  gate evaluates, so `--seed fix-budget=4` sizes one run without touching the
  file. The resolved value must be an integer ≥ 1 — an unresolvable or
  malformed bound is a `wiring-error` park (or fails the human's reject
  action loudly on the `reject.max` path), never a silent default — and the
  persisted decision carries the resolved integer, so derived loop counts and
  recorded budgets always compare the same numbers and a run's trace says
  what budget it actually ran under. Frozen snapshots that carry the old
  integer form decode unchanged.

- **D43. A fan-out unit may declare `resources:` of its own.** Phase-declared
  resources are attempt-scoped by design (a `live-stack` mutex is taken once,
  not once per unit), which left no way to put a per-unit cost under capacity
  accounting — the concrete case being a gate-check command unit that needs
  one `container-slot` per running check. Unit-declared resources are
  acquired per running unit through the same all-or-nothing, live-profile,
  shared-FIFO admission a phase uses; agent and tool units alike may declare
  them, with provider capacity still appended only for agent units. A call
  unit runs no work and declares none — validation refuses the declaration
  statically, and a frozen definition carrying one parks `wiring-error`
  rather than being silently ignored.

## An envelope owes its status, absence crosses call edges, and no park is immortal (2026-08-09)

The same overnight campaign, second cluster — this one diagnosed from the
production database with the run ids in hand. The original campaign root
turned out to have died twice by the same class of defect: the system treating
a legal absence as an error. A fully successful merge join was refused because
its hand-written envelope omitted the `question`/`reason` keys, and the
restarted root then parked at its own recursion point because an `optional:`
input was, as declared, absent. Two more were authority gaps a run could not
escape: a parked run was uncancellable from every surface, and a third reject
on a spent reject budget silently destroyed the gate's still-declared approve.

- **D44. Post-validation owes literal presence to `status` alone.** The
  generated envelope schema lists every control key in `required` because
  provider strict mode demands it — but `ValidateEnvelope` also judges
  envelopes the tool driver reads from disk, hand-written by scripts that
  never saw that schema. Requiring the keys literally made `"question": null,
  "reason": null` boilerplate whose omission converted a successful merge
  into an execution failure, a failed join, and an `agent-error` park. Now an
  absent `outputs` / `question` / `reason` / `narrative` reads exactly as the
  null a schema-bound provider would have sent — absence and explicit null
  are the same statement — while a `done` envelope whose phase declares
  outputs is still refused per missing declared output, by name. The rule the
  `narrative` field already had (D40: schema-required, absence-tolerated) is
  now the rule for every control key but the discriminator. One contradiction
  this resolved in the documented contract's favor: a control-only unit's
  envelope was documented as "outputs present but always null" while
  validation rejected exactly that on `done`; the `must be non-null when
  status is done` finding is gone, subsumed by the per-output findings where
  outputs are declared and ceremony where none are.

- **D45. An absent optional input crosses a call edge as absence.** Call
  arguments now evaluate where the resolved child is in scope: an arg whose
  reference does not resolve is omitted when the child input it seeds is
  declared `optional:` — the child sees what a direct start without that seed
  would give it — and refuses only when the input is required or undeclared
  (undeclared stays an error: nothing exists to be optional, and static
  validation already rejects the argument). One implementation serves the
  phase edge and the fan-out unit's call edge. The refusal also moved onto a
  persisted phase attempt row: the campaign's park happened during argument
  evaluation *before* the row persisted, which is why a run that visibly
  decided `advance → next-wave` rested `wiring-error` with no attempt to
  explain it. A refused unit call likewise leaves its `pending` row — a call
  that cannot be made is still not a unit failure.

- **D46. A parked run is cancellable.** `cancel` only reached items resident
  in the engine's memory, and the FSM allowed `needs-human → running` only —
  so a run parked at a gate nobody intended to approve was immortal short of
  resuming it into work nobody wanted first. `needs-human → cancelled` is now
  a legal edge; cancelling a non-resident run loads the parked record, walks
  its call children with the same store-driven walk teardown uses, and
  settles the tree — pure bookkeeping, since a parked run holds no processes
  and no resources. The parked attempt row stays untouched (it is the only
  record of why a human was asked). A parent whose called child was cancelled
  observes it through the normal child-settlement path: a call phase parks
  `agent-error` naming the cancelled child — cancelling the parent too is the
  human's call — and a call unit fails, parking `unit-failed`. A
  `disposition` park alone refuses cancel: that run is done; the disposition
  verbs settle it.

- **D47. A spent reject budget refuses the reject; it never converts the
  park.** The reject loop's bound could be exhausted while the gate still
  declared its approve, and the third reject converted the park to
  `retries-exhausted` — the gate offered two verbs and taking one of them
  destroyed both. An over-budget reject is now refused with the live options
  named: approve, `run resume --phase <target>` (a fresh entry from outside
  the cycle, which refills the loop bound), or cancel (real since D46).
  Nothing is persisted by the refusal — no intervention, no trace rewrite;
  the run rests exactly as parked and approve still completes the attempt.
  Making the named escape true required one adjacent fix: a human-gate park
  had refused `Resume` unconditionally, so the refusal would have named a
  dead verb. The guard now blocks only a resume in place or at the parked
  phase itself — the decision belongs to `ResolveHumanGate` — while naming a
  different phase is the human abandoning the gate to redo earlier work.

- **D48. Resume continues and preserves; `--phase` starts over; a failed
  join is a failed unit (user ruling: full flexibility, sane defaults,
  obvious usage).** The incident: a fan-out's working units were all done —
  each a completed call child, an entire child run — when the join failed
  (on the D44 boilerplate defect). The join was classified `agent-error`,
  the retry verbs refuse joins ("the join settles with its attempt"), and
  `recoverableUnitPark` excludes agent-error — so the only verb that worked
  was bare `run resume`, whose non-resumable-reason path is a fresh attempt:
  full wave re-expansion, completed child runs respawned from scratch. Worse,
  bare resume on an ordinary `unit-failed` park took the same destructive
  path — the generic verb an agent reaches for first silently discarded
  exactly the work the retry verbs preserve.

  Three rulings, one teachable rule (*resume continues and preserves;
  `--phase` starts over; retry verbs target units, the join included*):

  - **A failed join parks `unit-failed`, not `agent-error`.** The join is a
    unit of the attempt; its failure is a unit failure. Retry (single or
    all-failed) re-runs the join alone over the preserved unit results; drop
    stays refused — the join is what consolidates the units, so its absence
    cannot be accepted. `run status` lists the join among `failed-units=` so
    it is nameable by `run retry-unit`.
  - **Bare `run resume` never discards finished work.** `ContinuableReason`
    (`paused | interrupted | checkpoint | unit-failed`) routes a bare resume
    through the continuation path — reopen failed units and the join, keep
    done units and their call children, join on the thread it parked on. The
    dispatch lives inside the engine's `resume`, so no caller can reach the
    destructive path by accident. Everything else keeps fresh entry: gate and
    question parks rest on a *settled* attempt (join done), so continuation
    would be wrong for them, and the human-gate guard (D47) runs before the
    dispatch.
  - **`run resume --phase <id>` — which may name the parked phase itself —
    is the one explicit "start over".** Fresh attempt, wave re-expanded,
    call children respawned, loop budgets refilled by the fresh entry.
    Discarding finished work always requires saying so; the prior attempt's
    rows stay as history.

  Deliberate residue, recorded rather than papered over: a JOIN that rests
  `question` / `stuck` / `stalled` / `transient-exhausted` keeps its own
  typed park (those states have their own verbs — answer, takeover, the
  transient retry layer), and bare resume from those still re-expands. If a
  live campaign hits that footgun the fix is extending continuation to
  settled-join-absent parks, not reclassifying the states. Rider shipped
  with this cluster: discard now settles parked tree members too (the
  StateRunning filter predated D46; only a `disposition` park is skipped),
  and the frontend's existing "Retry unit" button now works on a failed join
  — its "Drop unit" label and the missing resume row in the unit-failed
  action group are surfaced follow-ups, not shipped here.

## A lane branch is named by its whole coordinate (2026-08-09)

- **D49. A fan-out unit's branch name keys on the full provisioning
  coordinate, not just `(item branch, unit id, try)`.** Wave 3 of the live
  campaign parked all three lanes `setup-failed` before any agent ran: a
  self-call child inherits the caller's workspace (§9), so every wave's run
  shares the root's branch — and wave 3's fan-out derived exactly the lane
  branch names wave 2's fan-out already created (`<root-branch>-port-0-1`,
  …). Lane retirement removes checkouts, never branches (deliberate — the
  rows are the cleanup source of truth), so the cut refused. Every
  multi-wave campaign would hit this at wave 3+, and the under-keyed name
  hid two latent collisions besides: a `--phase` re-expansion (attempt 2,
  tries reset to 1) derived attempt 1's exact names and would silently
  *adopt* attempt 1's checkouts as fresh lanes, and two fan-out phases
  sharing unit ids would collide within one item. The name is now
  `<itemBranch>-<owner item id, 8>-<phase>-a<attempt>-<unit>-<try>` —
  human-readable, no hashes, sanitize-then-truncate on author fragments —
  which keeps the two load-bearing properties: same coordinates → same name
  (re-entry adopts its own checkout; a call-bound unit's child lands in the
  checkout the unit owns) and the `<itemBranch>-` prefix (a run's branches
  stay findable from the item). Nothing else derives lane names — retire,
  discard, and enrichment all read the persisted unit row — so rows carrying
  old-format names need no migration. Known edge, loud not silent: two
  ~64-char author ids on a long item branch can exceed the loose-ref
  filename limit, failing the cut into a `setup-failed` park; bounding the
  fragments would reintroduce the collision class, so it stays.

## A usage limit is a schedule, not an outage (2026-08-10)

- **D71. Quota refusals park with the reset time and resume themselves.**
  Live-campaign feedback: a dated usage-limit refusal ("try again at Aug
  15th 7:56 PM") burned the whole transient backoff ladder in seconds,
  parked generic `retries-exhausted`, and waited days for a human alarm
  clock. Recognition is a PAIR, typed on both providers, never message
  prose: a refusal enum (Claude `assistant.error == "rate_limit"`; Codex
  `usageLimitExceeded`) plus the rate-limit windows the SAME session
  reported (Claude `rate_limit_event` `status: rejected` + `resetsAt`;
  Codex `account/rateLimits/updated` — Unix seconds on both, evidenced
  against both CLIs' sources). A recognized pair skips the remaining
  ladder and parks `retries-exhausted` immediately; a refusal missing
  either half falls through to the ordinary ladder unchanged. Along the
  way a real drop was fixed: AO's Claude parser discarded the rejected
  snapshot (it gated on `utilization`, which the rejected path never
  sets), and `observe` recorded snapshots only outside backoff — both
  would have made the feature silently inert.
- **The park arms its own return.** `work_items.auto_resume_at` (v54,
  Unix ms, 0 = unarmed) is the single source of truth; an app-side timer
  (the engine holds no timers) fires a bare resume — a continuation of
  the SAME session per D70, not a fresh retry — at the earliest future
  boundary among windows ≥99%, plus 1–3 min of id-derived jitter so a
  wave does not burst. The cause is composed AFTER the schedule write and
  claims only what happened: "resumes itself at <T>" on success, the
  reset time alone (with "resume it yourself") when the write failed. A
  boot sweep re-arms after restarts (past-due rows fire on a 30s delay);
  every transition out of the park clears both column and timer; a failed
  fire re-arms at 5 min and each fire re-checks resumability, so a
  since-repaired run clears itself. `run resume --at <time|+dur>` arms
  the same mechanism manually on any continuable park.
- **An outcome nobody authored an account for carries the runner's**
  (`Outcome.Detail`). An execution failure resting with an EMPTY envelope
  used to leave no cause, no envelope, nothing to diagnose from
  (live-campaign report: a lens death with a bare `execution-failure`).
  The runner now fills a bounded per-attempt failure detail at every
  failure exit, and the engine writes it as the park cause ONLY when the
  envelope is empty — an envelope with content stays the sole account.
  A latent deadlock fixed in the same area: `stopAndFinish` called from
  the provider event pipeline blocked on an interrupt whose response
  could only arrive through that pipeline; parks from the observe path
  now stop off-wire.

## The observer stops believing everything it hears (2026-08-12)

Root-caused from a zombied campaign wave: an `audit-fix` phase sat
`running` for hours after its turn had completed with a valid envelope.
The chain — a collab CHILD thread's `serverOverloaded` error read as the
phase turn's own failure, a retry ladder armed against a turn that was
alive, the retry absorbed by Codex as mid-turn input (minting a turn id
nothing ever starts), and the real completion dropped by the
retry-start filter with no timer left armed. Four doctrines fell out,
each pinned by an incident-replay test:

- **D73. A child's events are the parent turn's activity, never its
  signals.** Both adapters stamp `ParentToolUseID` on everything a
  subagent or collab child emits, and the observer now filters on it:
  nothing parented may enter the retry ladder, trigger the quota park,
  answer the turn start a retry is waiting on, or be consumed as the
  turn's completion. The one exception is read off the meta, not the
  provider: a parented error carrying `expect_turn_complete` IS the
  parent turn ending (a Claude Task-subagent's `assistant.error` closes
  the parent's open turn), and filtering it would downgrade a retryable
  rate limit — and its D71 self-resume — into a bare execution failure.
  The watchdog reset is the one thing every filtered child event keeps:
  a delegating turn leaves the parent stream quiet for as long as its
  children work, and that quiet is not a stall.
- **A Codex error is information; the completion is the verdict.**
  Codex core always terminates a turn with `turn/completed` (`failed`
  when it errored), so the transient ladder now arms from the
  empty-payload completion, never from the error notification — the
  same waits-for-completion path Claude's `expect_turn_complete` errors
  take. The ladder can therefore never arm while a turn is alive, which
  is what previously let a retry be swallowed as queued input; and a
  completion that arrives carrying an envelope self-corrects a bogus
  error instead of being discarded.
- **A live attempt always holds an armed timer.** Every wait the
  observer can enter is bounded by the inactivity watchdog now: the
  retry-start wait (armed at dispatch, before the send can land), the
  session-error wait for a disconnect that may never come, the
  waits-for-completion error path, and — at the send chokepoint
  (`sendIfActive`) — the opening and envelope-feedback sends, which
  previously left a Codex attempt timerless until `turn/started`. A
  wait that outlives the watchdog parks `stalled`: loud, resumable,
  and honest, where the incident's shape was eternal `running`.
- **Replayed turn lifecycle is identity-checked, in both directions.**
  A `thread/read` replay re-emits turn lifecycle, and the adapter's
  start-side dedupe forgets a turn id at that turn's completion. A turn
  start arriving while a turn is already started is therefore a replay
  and moves nothing; a completion naming a turn other than the one the
  attempt started (`currentTurnID`) is a ghost and finishes nothing.
  The window before any identity exists is covered too: every Codex
  send — opening, envelope-feedback, retry — enters the
  `awaitingTurnStart` wait, so no terminal event is consumed until the
  send's own `turn/started` names the turn it belongs to (unless that
  start already arrived, which the flag's `turnStarted` condition
  records). Claude names no turns, so all of it is inert there by
  construction rather than wrong.
- **A latched session death owns what happens next, and a queued send
  is valid only for the ladder state that queued it.** A session error
  arriving during a backoff window is latched (the window previously
  swallowed it, converting the ladder into an `agent-error` park), and
  a latched death suppresses the held resend at both ends — `timerFired`
  skips the send and keeps the watchdog, and `sendIfActive` treats the
  attempt as inactive — so `sessionDisconnected` folds the death into
  the ladder's next rung instead of racing a send into a dying process.
  The latch alone cannot catch a resend that was queued before the
  death and dispatched after the disconnect already answered it, so
  every send also carries the `sendEpoch` it was queued under and
  `scheduleTransientLocked` — the one place a rung is armed — advances
  it: a superseded send matches nothing and is dropped.

## The steer and the wake stop trusting luck (2026-08-10)

- **D72. An undecodable guidance slot heals instead of bricking.** The
  pending-guidance column is engine-written JSON, but a slot that will
  not decode used to park `wiring-error` at EVERY fresh entry with no
  clearing verb — an immortal park loop. Now it costs one loud park: the
  raw bytes are quarantined to the engine log (`guidance-undecodable`,
  never truncated — that line is the only surviving copy), the column is
  cleared, and the cause states all three facts plus "re-issue the
  steer"; the next resume proceeds. A failed clear stays an unhealed
  error that promises no repair. `Guide` over a corrupt slot heals and
  KEEPS the caller's entry (the corrupt bytes were unrecoverable either
  way; the caller's steer is the only one still live) and reports the
  quarantine on its own result — `GuidanceState.Quarantined`, rendered as
  a `warning:` line by `run guide` — because a generic error toast on a
  call that succeeded would be false and invisible to the CLI caller who
  actually lost the slot.
- **A wake is recorded as delivered only once it is durable.** The live
  branch used to persist the coalescing signature at hand-off to an
  in-memory queue; a session teardown between queue and dispatch lost
  the message while the signature said delivered — suppressing the
  identical re-park forever. The hand-off now writes a `queued:` claim
  and the dispatch callback promotes it by compare-and-set; any clear
  spends the claim automatically (the invalidation lives in the column,
  so no future clear site can miss it), and a stranded claim can never
  suppress — it matches no real signature. Descendant-park wakes also
  stopped carrying the root's declared workflow outputs (stale
  carry-forward values re-announced on every park deep in a campaign
  tree); the descendant's own attempt outputs are the message.
- Read-path residue from the same round: `scopedRun` and the watch tree
  walk moved off full-row reads (`GetWorkItemSummary` / a new
  `WorkItemNode` projection with no snapshot join); the two verbs that
  genuinely need seeds/budget/snapshot fetch the full row themselves.

## An API error costs a resume, not a re-run (2026-08-10)

- **D70. `retries-exhausted` joins the continuable parks.** User-reported:
  a phase turn runs many minutes, dies on a provider API error, and bare
  `run resume` threw the session away and re-entered fresh — while
  `interrupted` (app crash), the same shape of stop, continued on the
  parked session, and the transient retry layer itself was already
  re-sending into that same live session between backoffs. Now a bare
  resume routes it through the existing continuation path: next turn on
  the parked attempt's thread with a continue message, dead-session
  fallback to a fresh attempt with a note, `--phase <id>` the explicit
  start-over (and the one place `--refresh-def` applies, per the
  continuable-park contract). Investigation verdicts recorded:
  - **Non-allowlisted execution failures park `agent-error`, not
    `retries-exhausted`** — and `agent-error` stays fresh on purpose: it
    is a shared bucket (envelope-validation exhaustion, envelope decode
    failure, tool-phase failures, sentinel-less start failures, a
    cancelled call child, unknown outcomes) where continuation is wrong
    for most members.
  - **`retries-exhausted` is itself two causes** — transient exhaustion
    AND a gate's spent loop/reject bound — and the continuation covers
    both deliberately: neither the old fresh entry nor the continuation
    refills a bound (only entering the loop's target from outside the
    cycle does), so the shapes re-park identically, and the continuation
    is strictly cheaper — a fan-out continues its join instead of
    re-expanding the wave, a call phase re-links its child instead of
    starting a second run. The resume note is worded for both.
  - Riders verified with tests rather than assumed: loop budgets refill
    nothing on the continuation; `run amend`'s effect note stays truthful
    (a single-shape continuation rebuilds variables from the row, so an
    amended seed IS read; fan-out/call parks still report the fresh-entry
    route); pending guidance stays pending (a continuation is not a
    boundary); the join's transient exhaustion continues through
    `continueFanOutJoin` over the units' kept results.
  - `stalled` deliberately not included: the watchdog gave up on a session
    nobody can characterise, and a continuation may re-enter a wedged
    session — its own future decision. `stuck` untouched: an
    agent-authored clean ending, not an execution death.
  - Surfaces swept to the new truth: composer repair map, wake repair
    sentence, `run resume`/`run guide` usage pages, and the overlay's
    resolution row — `retries-exhausted` moved from the `blocked` kind
    (whose copy says "the phase starts over") to the `paused` kind, whose
    copy is exactly what is now true. The refusal message enumerates the
    continuable list programmatically, so a future member cannot join the
    rule and miss the message.

## The budget stops lying (2026-08-10)

- **D69. One spend fold serves enforcement, display, and the prompt.** The
  five-lens finding said "budget blind to ~70% of spend"; the investigation
  found attribution ALREADY worked (Codex workflow turns land in the ledger
  with `work_item_id` via the thread-keyed resolver, ordered safely because
  triage handles the event before observers can detach the registration) —
  what was missing was dollars: the budget sum read wire cost only, and
  every Codex row carries none. Fixed by composing the tree-spend read
  through the same query-time pricing `GetUsageStats` uses
  (`priceUsageGroups` / `ledgerSpend` — one fold, one rate table, three
  consumers: usage stats, the run-cost overlay, and
  `workflowSpendSource.TreeSpend` → `ResolveBudget` → both `checkBudget`
  and the display). Spend now says how much of itself is estimated and how
  many rows even the rate table could not price; an unrecognized
  `cost_source` fails the read loudly rather than silently subtracting
  from a total. Semantics per ceiling kind: a token ceiling ignores both
  caveats (tokens are exact); a USD ceiling not provably crossed refuses
  judgment on unpriced rows (`BudgetView.Unjudged` is a FIELD, and
  `checkBudget` is the one place it becomes an error — returning an error
  from the read took `run status` and the binding away over exactly the
  fact the operator needed, and the old in-source refusal parked
  token-budgeted runs `setup-failed` on any model the rate table hadn't
  learned); a proven breach outranks the caveat and parks for its budget.
  New surfaces, all resolving through the same call so they cannot
  disagree: the `budget=` line on `run status`/`run inspect` (absent
  without a ceiling), the truthful budget-exhausted wake number, and the
  reserved read-only `budget` prompt binding — {kind, ceiling, spent,
  remaining, estimated} in the ceiling's own units, unbound when no
  ceiling exists (declared `optional:` so it renders "(not provided)"),
  refused at every declaration site by the shared reservation predicate
  and refused in gate predicates (routing on spend is arithmetic in a
  predicate — the anti-change list holds). Accepted cost, stated in the
  guide: a budgeted run pays a second tree-spend aggregate per attempt for
  the binding; an unbudgeted run never touches the ledger.

## Authoring stops fighting the author (2026-08-10)

- **D65. Reserved `call-depth` read.** The engine's `CallDepth` counter,
  bound read-only into every element's variable context (root = 0) —
  killing the model-computed wave modulo that desynced in the live run.
  Named `call-depth`, NOT bare `depth`: authors already use `depth` (this
  repo's own test fixtures do), and the reservation is pinned in both
  directions. Refused at four declaration sites including **phase id** —
  a defect the packet found reviewing its own work: a phase named
  `call-depth` produces `call-depth.<output>` references while the engine
  binds the bare name last, so the reserved int would silently overwrite
  that phase's entire output object (the same both-ends collision
  `history` refuses; the fix's predicate covers `budget` too). Bound
  AFTER seeds deliberately: a caller's `args:` are evaluated, not
  validated against the reserved list, so a seeds column can carry the
  name — the engine's answer must win. The campaign starter keeps its
  `wave-number` seed with a comment: a campaign restarted as a fresh root
  (which the live campaign was) has depth 0 while its wave numbering
  continues — different facts, both available.

- **D66. `workflow validate` derives scope from where the file sits.**
  Investigated per class against reproductions: the id-prefixed-siblings
  class was not reproducible (both forms already resolved siblings from
  the resolved path's directory); the scaffold-rename class was already
  fixed by the every-sibling-prefix change (pinned anyway, over every
  starter); the real break was the path form computing NO scope — no call
  resolver, no profile bindings — so a project-scoped definition
  validated by path produced phantom `call.target` findings its `--id`
  form never showed. `workflowScopeForPath` now derives scope from the
  file's location under the config root (project dir → that project,
  winning over `AO_PROJECT`; shared dir → shared, keeping the caller's
  project for target checks), `--project` works with a path, and a
  round-trip test validates every embedded starter by both forms. Rider
  the fix exposed: three pre-existing aocli tests passed only because the
  path form was scope-blind — they were reading the developer's REAL
  config root and project profile; they are now hermetic
  (`--config-root` + a scrubbed env) rather than the feature weakened.

- **D67. Workflow outputs can be `optional:`, and required ones must be
  producible.** Incident D-C1: the campaign YAML declared an output
  sourced from a phase that never runs on the completion path, and
  `childOutputEnvelope` fails a missing declared output — the campaign
  dies at the moment it completes. Both halves shipped: `optional: true`
  omits an absent output exactly as D45 crosses an absent optional call
  arg (never a null; the call-unit path shares the code), and dry-run
  gains `workflow.output-unreachable` for a required output whose
  producer is not on every path to `done` — naming the output, the
  phase, one witness path, and both repairs. A human gate's approve
  target counts as a completion exit. Deliberate silences: optional
  outputs are never reported, and an unreachable producer is not
  double-blamed (`graph.unreachable` already owns that line).

- **D68. Phase inputs inherit workflow input schemas.** ~40% of the
  campaign's YAML re-typed schemas the workflow inputs already declared.
  A phase/unit/join/call-arg input bound to a declared workflow input by
  bare name with a zero-value schema inherits the whole schema
  (multiline, description, required, enum) at `Parse` — the one
  authored-bytes→Workflow transition, so it freezes with the snapshot.
  Explicit schemas win, unchecked for compatibility on purpose
  (narrowing is the point; the producer/consumer check still refuses a
  non-narrowing restatement). The only new refusal is none.

## The lane sees the campaign, the merge accounts for every lane (2026-08-10)

- **D63. Goal chain + `non_goals:` in every element's prompt.** The live
  campaign's lanes worked blind to the big picture — scope drift is the
  characteristic autonomous-planner failure, and "done" was the planner's
  opinion re-formed each wave. A workflow declares `non_goals:` (≤12 ×
  ≤500 runes, bounds are findings not trims, frozen with the snapshot);
  every agent element's prompt opens with the goal chain root-first —
  resolved app-side by the SAME ancestry walk the memory digest uses (the
  digest resolver was changed to take the resolved tree, so it is
  genuinely one walk) — with the middle elided past six links, consecutive
  identical goals collapsed to one link attributed to the root-most
  stater (a call copies the caller's goal verbatim onto the child, so a
  40-wave chain is 40 copies of one sentence), and root non-goals riding
  down only when they differ from this run's. Everything untrusted-quoted
  and labelled as data. Goals stay run-owned (`--goal`, call args);
  non-goals stay def-owned — not conflated. The acceptance-criteria
  ledger rode in as CONTENT with zero engine change: criteria as a typed
  root input, per-wave `coverage` output
  (`{id, state: uncovered|covered|satisfied|regressed, evidence, lane}` —
  `regressed` being the state a 40-wave port actually produces) forwarded
  through the self-call args like the wave number, dry-run-checked in the
  campaign starter.

- **D64. `accounts_for_units:` — the merge join must account for every
  lane.** Wave 4's hand-written merge script stopped at the first
  conflict and silently dropped an approved lane (~1900 lines nearly
  lost); a hand-written join envelope caused D44's incident. The engine
  still merges nothing — policy stays in the author's script — but a join
  declaring `accounts_for_units: true` must declare `merged` +
  `blocked` outputs (static finding, blamed on the phase since a join
  declares no outputs of its own), and a `done` join envelope is refused
  unless merged ∪ blocked is exactly the unit set: missing unit named,
  unknown/duplicate refused, blank reason refused, malformed entry
  refused where it sits. The refusal is D44's ordinary
  validation-feedback retry — the e2e test drives a join that drops a
  lane, gets refused naming it, and completes on the corrected envelope —
  never a park; D48's join-failure semantics are untouched. The reference
  starter script (`merge-unit-branches.py`) skips-and-continues on
  conflict and emits the contract. JSON Schema cannot express a
  partition, so the generated schema is unchanged and the engine owns the
  verification, per D44's post-validation doctrine.

- The composed convergence pattern the campaign invented as prose is now
  the `converge-on-review` starter: review reads `history.<phase>` (D51),
  emits a typed verdict, fix edge loops `session: continue` (D60),
  round-3+ narrows via `prompt:` (D61) with minors becoming residue
  through `memory add` (D57), the continuing route carries `notify: true`
  (D54 — on the LOOP route, not the terminal one, where it would be an
  inert report; the packet corrected the orchestrator's brief on this).
  Starters all pass `workflow validate` in a test; the scaffolder now
  id-prefixes every non-`workflow.yaml` sibling so two scaffolds into one
  scope cannot collide on a script name.

## Loops get a temperature and runs take direction (2026-08-10)

- **D60. `session: continue | fresh` on loop routes.** The measured
  ping-pong: every loop re-entry ran cold, so round N lost what round N−1
  tried. `continue` runs the re-entered attempt as the next turn on the
  target phase's newest provider thread, feedback as the message — riding
  `PriorThreadID`, the exact field `run answer`, resume-in-place, and
  takeover-finalize already set, consumed by the one runner-start path. No
  second same-session mechanism. `fresh` stays the default (anti-anchoring:
  review edges re-enter cold by design; starters set `continue` on the fix
  edge). Statically refused on non-loop routes and on loops targeting
  tool/call/fan-out phases (no single session to continue — the finding
  names which). A dead prior thread degrades to cold with a feedback note
  and an engine log line, never a park: `applyLoopRoute` runs after the
  deciding attempt's teardown, so an error there could only become a park —
  an unavailable optimisation must not be an outage. No new column: two
  attempts sharing a thread id IS the provenance, rendered
  `session=continued` on `run status`; it deliberately does not distinguish
  the loop knob from an answer or a takeover — all three mean "this round
  remembers the last one", and the definition says which edge asked. The
  knobs are ignored on the loop a `human:` reject synthesizes (scoped out
  explicitly, test-pinned) — a reject edge wanting warmth is a future knob,
  not an accident.

- **D61. `prompt:` on loop routes — per-round narrowing as syntax.** The
  live campaign's working convergence ratchet ("round ≥3: new minor
  findings become residue") lived as unenforced prose in one prompt file.
  A loop route may now name a prompt file the re-entered attempt renders
  instead of the phase's own: sibling-relative, template-checked against
  the TARGET phase's inputs, inlined and frozen with the snapshot,
  re-read by `--refresh-def` — one rule set with phase prompts
  (`validatePromptFile` extracted to share it). Route-scoped, never
  sticky. Refused against fan-out targets rather than silently applying
  to no unit. Composes with `session: continue`: the override becomes the
  continuation turn's message.

- **D62. `run guide` — the pending-guidance slot.** Steering a
  free-running run previously meant parking it or waiting for a park.
  Guidance appends (bounded: 4 KiB × 8 entries, system-stamped author —
  human vs. the phase run that wrote it) to `work_items.pending_guidance`
  (v53, the v52 narrow-accessor pattern) and delivers at the run's next
  **fresh phase entry**: rendered as a labelled untrusted-quoted block,
  cleared on delivery, feedback-noted on the attempt. **Delivery
  atomicity chose redelivery over loss**: the attempt row persists first
  and the slot clears second, so a crash in the gap redelivers — the
  reverse order silently loses what the operator wrote, which is the
  worse failure; a failed clear parks `setup-failed`, the same safe
  direction. Continuation resumes deliver nothing (the attempt continues
  as it was) and `guidance-pending=N` on the run's status line is what
  keeps that visible — a bare resume that leaves guidance undelivered is
  exactly when the caller needs to see it. Promptless boundaries
  (tool/call phases) skip and retain. Fan-out units and the join carry
  the phase's delivered guidance. A called run is guided directly (D59's
  row-ownership truth). Mid-turn injection remains explicitly out of
  scope — the slot is the `when_done` half only, per the standing
  deferral of the correction flow.

## Waiting and repair stop costing tokens (2026-08-10)

- **D58. `run watch`: the wait is server-side, the wire stays one POST.**
  The live campaign's supervisor hand-rolled 7 while-true monitors (one
  died silently and cost a night), 712 log-tail polls, and 4h of sleep,
  because no verb could WAIT on a run. `run watch <id> [--tree]
  [--timeout]` prints one line per state transition and ends with the
  resting state plus the wake's own repair sentence verbatim (a CLI that
  reworded it would be a second answer to "which verb settles this").
  Mechanics: scoped tokens have no WebSocket — and giving them one for a
  single verb would mean a replay ring, a channel filter, and a second
  wire — so `WorkflowAgentWatchRun` **blocks server-side** on the same
  state listener the wake uses, holding ≤25s per call under the CLI's 30s
  RPC timeout under the transport's 60s write timeout, so the client is
  always the one still waiting and a revoked credential surfaces as a 401
  within one hold instead of a hang. A bounded in-memory sequence ring (a
  jitter buffer, not a history store) feeds it; a cursor the ring cannot
  honour prints as a `gap` rather than skipping silently, and a first call
  starts at head — the caller asked what happens NEXT, and replaying
  history into an agent's context is the opposite. Exit codes are the
  point: 0 done, 1 rested elsewhere, 2 refused, **3 timeout expired, 4
  the app stopped answering** — because a supervisor branching on the exit
  has to tell "the run rested" from "I stopped looking" from "I was cut
  off", and collapsing those is the silent-monitor failure the verb
  deletes. Building it surfaced three would-be bugs, each now tested: the
  CLI's error path collapsed exit 4 into 2; cursor 0 meant both "no
  history" and a legitimate fresh-app head (busy-loop); and the first call
  falsely reported a gap.

- **D59. `run amend --seed k=v`: the seed counterpart of `--refresh-def`.**
  Changing one seed value on the live campaign cost $14/20M tokens
  (cancel + respawn). Amend edits only the keys it names on a RESTING
  run's row — refused running (an attempt reads seeds at start), refused
  terminal, refused for undeclared keys naming the declared ones, each
  value validated by `def.ValidateInput`, the per-key half of the intake
  validator, so a value accepted at start and one accepted later cannot be
  judged by different rules. The output states WHEN the run reads it,
  because the answer differs: an ordinary park's next attempt renders it,
  while a fan-out/call park repaired in place by bare resume runs on the
  variables its attempt froze — so the note names `resume --phase` as the
  fresh entry. **A called run may be amended** — the orchestrator's
  assumption that a child's seeds are caller-owned was checked against the
  code and found wrong: seeds are not in the frozen snapshot,
  `variableContext` rebuilds them from the run ROW at every phase entry,
  so a called run's remaining phases genuinely read its own row; the
  result notes that the caller's `args:` re-evaluate at the next
  invocation and will not carry the change, naming the root. Deliberately
  NO feedback note on the attempt: `--refresh-def` writes its note at the
  moment of entry, but an amendment happens while the run is evicted, and
  writing into a prior attempt's `input_envelope` would falsify the record
  of what that attempt actually ran with — the seeds column, an engine log
  event, and the CLI's own output are the durable evidence.

## The tree remembers (2026-08-10)

- **D57. Campaign memory: one append-only log per root run, app-owned,
  auto-promoted.** The live campaign's lanes relearned the same environment
  quirks and porting patterns wave after wave — knowledge died with each
  attempt, and the review loops' ping-pong was partly this. Design adopted
  from the orc investigation's knowledge layer, keeping its load-bearing
  constraints (closed kind vocabulary, system-stamped provenance, a format
  contract in the curator prompt) and refusing its two failures (heavy
  infrastructure nobody used — this is one NDJSON file; a human-graduation
  gate that made notes a write-only log — promotion is automatic, per the
  user's autonomy ruling). Mechanics:

  - **Storage**: `<configDir>/workflow-memory/<root-run-id>/notes.ndjson`,
    keyed by the run TREE's root via the existing ancestry walk, created
    lazily on first note. Outside the repo and the worktree — no cleanup
    debris on the deliverable branch (orc's C4 cautionary). `wave` is the
    writing run's existing `CallDepth` — no parallel counter. Appends are
    single-write; a torn final line costs exactly one note (the next
    append heals the unterminated tail — the first build had a
    poisoned-log defect where a tear welded the next note onto it, found
    and fixed in test), reads skip + count torn lines, and an over-long
    line fails the read loudly rather than silently dropping the rest.
  - **Notes are typed, bounded, and provenance-unforgeable.** Closed kinds
    `pattern | warning | learning | handoff` (`ruling` deliberately
    deferred, test-pinned absent); text 4 KiB, ≤20 cited files;
    `NewNote(draft, provenance, at)` is the only constructor and the draft
    has no provenance field, the RPC has none, and envelope entries refuse
    `provenance`/`at`/`wave` keys as unknown properties.
  - **Two write channels, the narrative precedent exactly**: `memory add`
    / `memory list` CLI verbs for write-capable elements and humans
    (row-level authority, no declared grant — writing memory is part of
    doing the work, like the envelope; a new `GrantNotRequired` transport
    marker says so explicitly, and `def.KnownGrant` cannot declare it), and
    an optional `memory` control field on EVERY element's envelope
    (required-and-nullable in the generated schema, like `narrative`,
    because strict mode means an absent field is one a provider cannot
    emit), validated per-entry with ordinary feedback retries, stripped at
    the narrative seam in both drivers — the tool driver lifts it too, so
    a `check:` that learned something durable has the channel. A write
    element's prompt says to null it, but an entry sent there is recorded
    rather than wasted on a refusal retry.
  - **Injection is the promotion.** Every element prompt carries the path
    (read-only sessions restrict writes, not reads — verified against both
    providers' enforcement, so the path line is honest in every mode) and
    a digest under an 8 KiB budget: handoffs newest-first, then the rest
    newest-first, grouped by kind, whole-entry aging, header always
    stating `N of M notes` and naming the log. Rendered live per attempt
    from the file — no rendered-brief cache to go stale. All text through
    `untrustedtext`.
  - **Lifecycle**: survives completion AND discard (discard deletes no run
    records — verified, matched), deleted by project deletion alongside
    the workflow records. Reading another tree's memory is not a
    capability: `introspect` deliberately does not widen it.
  - **Curator is content**: a read-only, cheap-model, end-of-lane distill
    phase in the `port-one-task` starter with the format contract ("write
    for an agent with NO context") and a do-NOT-add list — never an
    engine feature.

## The wake earns its interruptions (2026-08-10)

- **D54. `notify:` — a route decoration that wakes without parking.** The
  live campaign's supervisor had exactly two ways to hear "wave N finished,
  continuing": park the run at a gate it would always approve (30× lane
  cost in one measured experiment) or hand-roll while-true polls against
  the database (712 log tails). The user ruling — notify-not-gate at wave
  boundaries — becomes syntax: `notify: true` on any route that leaves the
  run running. The route routes unchanged; the bound thread gets a
  best-effort progress wake (run, phase, route, bounded outputs digest)
  that can never park, fail, or delay the transition. `human:`/`park:`
  routes refuse it statically (the park already wakes — a decoration there
  promises two wakes for one event); a terminal route takes it as a
  non-blocking report and fires nothing, because the resting wake already
  goes out and a second message is the duplicate D55 exists to kill. A
  called run's notify composes as the root's wake naming the descendant;
  unbound roots have no progress surface, so there it is inert (documented,
  not invented — an OS notification per wave is the interruption budget
  this cluster exists to protect). `ContinuesPastGate` is the one exported
  definition of "this decision leaves the run running", so a frozen
  snapshot carrying a decoration on a resting decision cannot fire it.

- **D55. Wakes deduplicate by what they say, never by when.** 55.8% of the
  live campaign's 43 wakes were verbatim repeats — one lane woke the
  operator ten times — because every resting transition composed
  unconditionally, and a crash rebuild re-parks every interrupted run on
  every launch. The fix is a content signature (run, resting state, typed
  reason, phase + attempt, question text, engine cause, and the same for a
  parked descendant), persisted on the run row (v52) as the last wake
  DELIVERED. Matching signature + nothing has happened on the run since →
  suppressed with a durable log line; any field differing → delivers. "Has
  happened" is recorded, not inferred: any tree member returning to
  `running` — every resolve, answer, resume, retry, rerun — clears the
  root's stored signature, so acting on a run re-arms its wake. One
  delivery seam (resolve binding → check → deliver → record) is the only
  path any composer reaches the thread through; a signature read error
  delivers rather than risking silence. Progress wakes coalesce by their
  own signature, which includes the attempt — without it a decorated
  loop-back would report wave 1 and swallow every wave after. Known
  remainder: OS notifications for repeated descendant parks are not yet
  coalesced — wakes only.

- **D56. The wake carries what deciding needs; the verbs carry the rest.**
  The measured tax: worktree/branch asked 12×, the parked phase's outputs
  read before every gate resolve. The body now carries the run's worktree
  path and branch (a descendant's only when they differ), and a
  needs-human gate park carries the parked attempt's outputs digest —
  built by the same bounding helper `run inspect` uses (one bounding, per
  D52's doctrine), overflow naming the literal drill-down command. The
  question bound rose 800 → 2000 runes: 800 truncated real gate questions
  mid-sentence. The composer stayed pure and was split (input / compose /
  closing / progress / signature) under the 500-line bar.

## Parks explain themselves (2026-08-10)

- **D53. Every engine-side park persists the engine's own diagnosis, on an
  attempt row, in its own column.** The five-lens evaluation measured the
  gap: a `setup-failed` / `wiring-error` / engine-`agent-error` park
  persisted no cause anywhere, so diagnosing one was an outage
  investigation (~12 tool calls against the database and the filesystem per
  park), and the worst class — a park before the first attempt row existed,
  like the original campaign root's fatal call-arg refusal — left *nothing*
  to investigate. Mechanics, in three parts:

  - **The cause is a column, not an envelope.** `work_item_phases.park_cause`
    (v51, bounded 8 KiB at a rune boundary). The half-built channel it
    replaces — `parkCauseEnvelope`, which wrote a forged
    `{status: "stuck", reason: <Go error>}` into `output_envelope` — is
    deleted: the envelope is the *agent's* artifact, and engine prose
    written there was read as something a model said by every consumer
    (the history binding reported `envelopeStatus: stuck` for a turn that
    never ran; the crash rebuild treated it as a terminal agent outcome).
    Teardown takes the cause as an argument; a non-park completion writes
    it empty, and reopening an attempt clears it — a stale diagnosis on a
    live attempt reads as a park that already happened again. Deliberately
    causeless: `taken-over` / `paused` / `interrupted` / `cancelled` / the
    crash sweep (the reason *is* the cause), gate parks (the trace is the
    record), and every agent-authored outcome.
  - **A park always has a row to rest on.** A phase known but never
    attempted gets its row created at the park (`parkOnNewAttempt`), and a
    run is placed on its first phase *before* the snapshot freeze, so
    every `parkUnstartable`, budget-check, and enter-phase failure now
    lands on a persisted attempt. The one deliberate exception: a run
    whose workflow never resolved has no phase to rest on — the cause goes
    to the caller and the engine log, and zero attempt rows is the honest
    record. The audit that produced this touched ~35 park sites and found
    one that parked with nothing at all (fan-out resume acquire failure).
    Accepted trade-off: loop-budget derivation sees the parked attempt
    where it previously saw none, so a resume through it under-refills —
    the direction that derivation is already required to err in.
  - **Surfaced everywhere a park is decided.** `run status` / `run inspect`
    attempt lines render a bounded `cause=` (untrusted-quoted); `run
    inspect --phase` prints it whole; both `--json` shapes carry it; the
    wake gains one bounded line ("The engine stopped it here: …") for the
    run and for a parked descendant, resolved from the resting attempt
    row; and the desktop run-detail pane shows it. `stuck` also joined the
    wake's repair map — `run resume <id>` (fresh entry; `stuck` is not a
    `ContinuableReason`), plus `--refresh-def` when the fix was an edit —
    so the one park an agent authors most often now names its verb.

- **D53a. The engine has a log stream.** `<configDir>/logs/engine-YYYY-MM-DD.ndjson`,
  same rotation and caps as the provider-event log but **no env gate** — a
  run parks once, and the evidence has to exist the first time. The engine
  writes park / cancel / resume / definition-refresh / rebuild / capacity
  events through a `LogSink` seam in its config (the engine does not import
  `internal/logging`; the app adapts), replacing the three bare
  `log.Printf` sites that previously scattered engine evidence into
  stderr. It is a maintainer surface — the operator-facing record stays
  the park cause above.

## Loops get memory, runs get a read surface (2026-08-10)

- **D51. A reserved `history.<phase>` binding carries a phase's prior
  attempts into prompts.** The five-lens evaluation of the live campaign
  converged on this as the single most expensive gap: `variableContext`
  collapsed phase history to the latest completed envelope, so review round
  N was structurally blind to round N−1 — two adjudicators alternately ruled
  opposite ways, each fix obeyed the latest, and three lanes burned 58% of
  all phase attempts ($216 of $563) before an operator ended the loops by
  fiat. The prose convergence ratchet an operator authored mid-campaign cut
  rounds from {13, 14, 11} to a flat 3 across nine lanes; this binding is
  the data that lets a definition express that ratchet properly (the
  adjudicate join reads its own history and emits a routable convergence
  output; the gate routes on it). Mechanics: declared like an input
  (`history.review: {schema: {type: array}, window: 6}`), engine-composed
  entries oldest-first excluding the current attempt, non-completed attempts
  as stubs — a deliberate, binding-scoped carve-out from "only completed
  attempts feed variables" — 32 KiB budget with whole-entry elision that
  names itself, `window` refused on any other declaration, unknown phase a
  dry-run finding, `history` reserved as a phase id and input name. Bound
  after authored variables like `units`, so it cannot be shadowed. Prompt
  surface only; predicates and workflow outputs cannot see it.

- **D52. The run's read surface is the CLI's obligation, not the
  database's.** The campaign's supervising agent ran 45 raw sqlite queries,
  79 hand-pathed narrative reads, and once `strings` on the shipped binary,
  because no verb exposed a run's worktree/branch (asked 12×), a phase
  attempt's envelope outputs (read before every gate decision), units with
  branches, children, or seeds. Shipped: `run inspect <run-id>` — the
  one-call picture (status + seeds + worktree/branch/base + children +
  per-phase attempt lines with a bounded output digest on each phase's
  latest attempt) with `--phase [--attempt]` drill-down to full outputs,
  gate decision, and units; `run narrative <run-id> --phase [--attempt]
  [--unit]` — prints the narrative through the same resolver the wake uses
  (a path is never derived twice); `seeds` on `run status --json`. Both
  verbs scope rows exactly as `run status` does and sit under the existing
  read grants — a wider view of a run the caller may already see is not a
  wider set of runs. Absence and wrong coordinates are distinct answers: a
  narrative that was never written is exit 1 with the path looked for; a
  phase the run doesn't have is a refusal naming what it does have, exit 2.
  The `--json` document shapes are documented in the aocli guide, so nobody
  reads the binary again.

## The freeze gets an explicit escape (2026-08-09)

- **D50. `--refresh-def` re-reads a parked run's definition at a fresh phase
  entry; nothing refreshes implicitly.** The snapshot freezes the whole
  resolved definition — prompt file contents inlined — and every attempt
  renders from it. That is deliberate (mid-flight reproducibility; the call
  edge is the designed re-read, which is how a campaign wave picks up
  edits), but it left a run parked for *operator repair* with no edit
  channel at all: a live lane parked `stuck`, the operator edited the phase
  prompt, resumed, and the lane re-rendered the frozen prompt into an
  identical park — twice. `run resume --refresh-def` and `run rerun
  --refresh-def` now re-resolve the definition from disk, re-validate,
  re-freeze the snapshot, and then take the verb; the re-frozen snapshot is
  the durable trace (a crash rebuild renders the new definition), and the
  next attempt's feedback note says the definition was re-read.

  The boundary rule: **a refresh happens only where a phase is entered
  fresh.** A bare resume on a continuable park is a continuation of an
  attempt whose units were launched under the frozen definition, so refresh
  there is refused toward `--phase <id>` — swapping the definition under a
  mid-flight attempt is incoherent. Refusals are total: a rejected refresh
  leaves the run record byte-identical. Refused outright: an entry phase the
  edit renamed or removed (enter it under its new id), and a workspace-need
  flip the run cannot satisfy — a run that recorded a worktree cannot go
  `worktree → project-root` (its work lives there), and a *called* run
  cannot go `project-root → worktree` (a child provisions nothing, §9);
  a root run that never cut a worktree may gain one, since the fresh entry
  provisions exactly as its first phase would have. The human-gate resume
  guard runs before any of this and is not bypassed. `started_at` is not
  re-stamped by a resume refresh — the wall-clock budget measures against
  it. No UI affordance yet; the frontend passes `false`, and exposing it is
  a product decision.
