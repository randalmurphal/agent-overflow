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
  The producer is an `ao` subcommand available to interactive threads
  (normal chat + triage), described to the agent so it knows the
  capability exists, but invoked **when the user asks** — the tool
  guidance must instruct agents not to enqueue unprompted. A settings
  toggle gates the whole feature (guidance injection + authorization);
  off means the capability is absent, not merely hidden. The §7.2
  confirm card remains the commit point — chat never enqueues silently.
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
