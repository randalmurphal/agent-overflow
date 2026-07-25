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
