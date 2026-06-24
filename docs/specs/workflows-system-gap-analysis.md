# Workflows System — Gap Analysis (punch-list for §1–§11)

> **STATUS: FOLDED IN.** All "Definitely add" items below are now in `workflows-system.md`
> (the teardown keystone is §12; the rest are edits across §2–§11). All four "Maybe" items
> were **declined** as not worth the complexity. This file is retained as the rationale +
> the anti-pattern guardrails to keep honoring.
>
> Source: a 6-lens parallel research workflow (durable-execution engines, agent
> frameworks, CI/CD, anti-patterns/"the bad", human-in-loop+notifications, authoring DX)
> → synthesis (21 deduped gaps) → **simplicity critic** (re-ranked by *concepts added*,
> cut to a minimal shortlist). 8 Opus agents, ~539k tokens. This is the accepted
> work-list to fold into `workflows-system.md`. Items are in the critic's minimal form,
> not the synthesis's fuller form.

## Keystone — one teardown contract, five triggers (the single biggest simplicity win)

Crash-restart, cancel, watchdog-trip, transient-retry-exhaustion, and budget-exceeded are
**not five features** — they are **one mechanism with five buttons.** Specify **one
teardown path** once: *stop the turn → release the phase's resource locks (§6) → write a
partial envelope → route to a terminal/needs-human state with a typed reason.* All five
triggers invoke it. Specifying it five times reinvites the leaked-`live-stack`-mutex
deadlock (the sharpest edge in the report) in whichever path is written last.

## DEFINITELY ADD (minimal form)

| # | Add | Section(s) | Minimal spec change |
|---|---|---|---|
| **0** | **Teardown contract** | §2, §6 | One defined teardown path (stop turn → release locks → partial envelope → route w/ typed reason); the keystone all below reuse. |
| **A1** | **Crash recovery — park, don't auto-resume** | §2, §10, §6 | On app start, any `running` item whose current phase has no terminal envelope → run teardown → park `needs-human(interrupted)`. Human re-runs via §7. (No checkpointing, no auto-resume.) |
| **A2** | **Inactivity watchdog (one facet)** | §2, §3, §6 | Per-phase inactivity timeout (project-profile default); no stream event for T → teardown → `needs-human(stalled)`. A watchdog over the stream the phase already emits — **no** wall-clock cap, **no** per-phase knob required. |
| **A3** | **Notify on park/fail + run-complete summary** | §7, §10 | OS notification on `needs-human`/`failed` only (deep-link to the phase thread) + one coalesced summary on run stop ("5 processed: 3 done, 2 need you"). Over the §11 internal-event stream — no new source. `done`/`queued`/`running` never interrupt. **Ship first.** |
| **A4** | **Cancel button** | §2, §6, §9 | Cancel on a running item → teardown → new terminal state `cancelled` (distinct from `failed`: failed = work failed, cancelled = human chose). Worktree persists. |
| **A5** | **Idempotency — surface-and-skip** | §5 | First-party side-effecting tools (enqueue/schedule/report-back) record what they did per (item, phase); on re-run the system **shows the prior effect and defaults to skip**, not silent magic-key dedup. Honest that arbitrary MCP side effects can't be made idempotent. |
| **A6** | **Pin run to workflow snapshot** | §8, §10 | At run-start, freeze the resolved graph + project bindings into the run record; the engine reads the snapshot, never live config. Edits affect only later items. (Conductor snapshot-at-start — **not** Temporal patching.) |
| **A7** | **Re-run current phase from recorded input** | §7, §10 | Extend §7 discard+re-run to seed the phase from its persisted §10 input envelope (cheap workflow-iteration loop). |
| **A10** | **Transient-execution retry (conservative)** | §3, §4 | A phase failing to *produce an envelope* due to a **small allowlist** of transient errors (process exit, provider-overload, network) → backoff, cap ~3 → `needs-human`. Unclassified → park immediately, never retry. Distinct layer from §4 validation loop-back (carries no feedback). |
| **A11** | **Extended static lint** | §3 | Add to the dry-run: (1) every gate outcome names an existing phase, (2) every phase reachable from start, (3) every loop-back declares its §4 bound, (4) every resource bindable by ≥1 profile, (5) variable refs resolve — each error naming the offending element. |
| **A12** | **Typed reasons + intervention field** | §2, §10 | Run record gains a **typed reason** on every `failed`/`needs-human` transition (stalled, budget-exhausted, retries-exhausted, check-failed-genuine, agent-error, wiring-error, interrupted) + a per-phase `intervention` field (kind/at/note for §7 take-over/complete/discard). Two fields; prerequisite for A3. |
| **A13** | **Report-back tool** | §5 | First-party `report-back` tool (outbound twin of §11's poll): a terminal phase shells the user's authed `gh`/`glab`/Jira CLI to post status to the seed's ticket key. Same delegate-to-host, no stored creds. |
| **C1** | **Per-item budget** | §6 | One optional per-item budget (tokens/$ if the provider reports it, else wall-clock), checked at phase boundaries → `needs-human(budget-exhausted)`. **Replaces** A2's max-turns — do not ship two runaway ceilings. |
| **C2** | **Secret references + narrative masking** | §8 | Project profile stores secret **references** (name → env var / keychain / file path), resolved into phase env/MCP at entry, **never persisted**; mask resolved values before the agent-written narrative is persisted. A reference store, not a vault. |
| **C8** | **Starter workflows + scaffold** | new (rel. §8) | Ship 2–3 example workflow files (`build-and-validate`, `multi-lens-review`, `poll-jira-and-start`) + a `new workflow` command that stamps a minimal valid graph. Content, not engine. |

## MAYBE — your call (judgment, not clear-cut)

- **A8 build-cache seeding** — real time-saver (cold `node_modules`/compiler cache on every
  item against a repo), but a caching subsystem is where "green-but-stale" bugs live.
  **Critic's verdict: do it as a user-authored setup-script phase (§3 scripts are
  first-class), not engine cache machinery.** Don't build invalidation into the engine.
- **A9 dynamic dry-run** (simulate gate/routing with canned envelopes) — valuable for
  complex graphs, but adds a fixture-authoring mode. **Defer until A11's static checks
  prove insufficient.**
- **C4 reconcile-at-join** (re-state the goal as a schema'd "is it met?" before a soft
  join/`done`, to catch confident-but-wrong envelopes) — **nothing to build; it's already
  expressible as phases+gates. Document it as a recommended pattern.**
- **C7 list-parameterized fan-out** (bind N units to a typed list var + fail-fast) — clean
  §3 extension *if you actually have the use case* ("run the per-file fixer over N changed
  files"); don't add speculatively. The cross-OS/version matrix grid stays cut (S1).

## CUT (complexity for marginal value at N=1)

- **A2(b) wall-clock backstop** — the inactivity watchdog covers it; a duration cap is
  "uselessly loose or kills good work."
- **A2(c) max-turns** — folded into C1's per-item budget.
- **C3 on-failure teardown *route*** — verify §4 conditional routing already targets a
  teardown phase on a failure outcome, then document; don't build machinery.
- **C5 off-desktop notification** — defer behind A3; add a one-line `ntfy`/Slack CLI
  shell-out only when overnight-away drains actually happen.
- **C6 supersede stale in-flight item (concurrency key)** — §11's cursor/watermark dedup
  covers the common case; the rare residual is handled by the dev seeing two items and
  cancelling one. Not worth a new config concept.
- **S1 matrix grid** · **S2 mid-graph/upstream-phase replay** (the worktree is mutated
  *forward*; a variable-only rewind desyncs from the code — `git reset` + fork-a-thread
  already does it manually) · **S3 multi-user approval governance** (one human).

## Anti-patterns to design against (preserve these guardrails)

- **Silence is the enemy** (the N=1 desktop throughline): never let a terminal state be
  reachable without a goal-check (anti-theater extended: "done" = validated, not "agent
  said done"); every park/fail surfaces actively with a typed reason (A12); `running` must
  always be exitable and cancellable (A2/A4 — §6's "blocked phase holds its slot" must not
  mean *forever*). The OS notification is a *nudge*; the durable needs-attention badge is
  the source of truth and re-surfaces on app open.
- **Don't build distributed-systems machinery**: no Temporal-style deterministic replay /
  patching (impossible for non-deterministic agent turns — record the envelope, never
  replay the turn); no heartbeat-RPC subsystem (local watchdog over the stream is enough);
  no dead-letter queue (the `failed`/parked states *are* it), no backfill, no
  DND/digest/escalation engine. **Author workflows as human-readable text files versioned
  by the repo's own git — never a visual builder.**
- **Don't re-fire side effects / gate everything / grow an expression DSL**: idempotency
  surface-and-skip on first-party tools (A5); any re-run defines its worktree starting
  point explicitly; reserve approval for irreversible *external* effects (`git push`,
  live-API MCP) via a tiny per-tool flag — never general per-call HITL (it destroys the
  autonomous-drain premise and becomes click-yes-to-everything); keep gates = pure routing
  over *typed* variables with a small checkable predicate set (no `${{ }}` logic-in-strings
  — if a decision needs real logic, it's an ai-judgment/check *phase*); keep §4's diagnosis
  phase mandatory before any loop (don't degrade it into a blind retry counter).

## Non-gaps (already covered — do not re-add)

Whole-queue pause/resume (§6); infinite handoff loops (the static, statically-checkable
phase graph *prevents* the #1 multi-agent failure — a **strength** worth stating);
dead-letter queue (failed/parked states); artifact-passing (envelope file-refs +
persistent worktree — only residual: confirm refs can point at *gitignored* build outputs
and surface in the run record).
