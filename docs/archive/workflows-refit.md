# Workflows product refit: the reconciler, gates, packets, and the landing train

Status: DRAFT for discussion — grounded in the 2026-07-14 BLITZ/ai-foundations
investigation. This re-centers the workflows product layer on the problem that
investigation surfaced; the engine (`internal/workflow/`) and the §11 automation
design in [workflows-system.md](workflows-system.md) are reused, not redesigned.

## 1. The problem, measured

ai-foundations, BLITZ board, 2026-07-14: 325 tickets. 148 Done, 140
Backlog/Selected, 22 In Review, 15 In Progress, 197 unassigned. Creation runs
~75/month sustained and July is pacing ~150. One person (with agents) created 81
tickets in 30 days while completing 30. ~2.5 humans review at human speed what
agents produce at agent speed.

The failure modes, in causal order:

1. **The board lies.** ~11 of 15 In Progress and 4 of 22 In Review rows are
   relics untouched for months; 7 open MRs are 19–69 days old, mostly
   superseded. Planning off this board is guesswork.
2. **Intake is unrefined.** PM stories arrive as one sentence plus an empty
   checklist template (Command Center epic: 6 such stories in one day, all
   High, all unassigned). Turning each into an implementable spec is senior-human
   labor that competes with review time.
3. **Ticket prescriptions go stale or are wrong.** Tickets are notes — the
   problems are real, the proposed solutions are sometimes wrong or superseded.
   Nothing re-checks a backlog item against the code that has landed since it
   was written.
4. **Review is the binding constraint.** 18 of the 22 In Review tickets belong
   to one author; 7 MRs from a single batch sat 5 days without movement.
5. **Waiting MRs rot into conflict debt.** At ~20 commits/day to main, an MR
   that waits a week needs its own project to land. Clearing today's open MRs
   is estimated at a week of fighting conflicts.

Agents multiplied production and discovery; triage, refinement, and review
stayed 100% human. That gap — not coding automation — is what workflows attack.

## 2. Product principles (binding)

- **Investigation is autonomous; writes go through the composer.** A finding's
  action becomes a pre-filled draft in a normal chat thread — the user reads
  it and presses send (or doesn't). Nothing auto-sends; the app itself never
  writes to the tracker or forge. Execution happens in the thread with the
  user's own CLIs, supervised like any other agent turn.
- **Reports live in Agent Overflow, never in Jira.** Jira is the team's shared
  notepad; agents do not talk over humans in it. The only Jira writes ever
  proposed are status-level: transition, assign, close, link-as-duplicate.
  Never description edits, never solution comments.
- **Reports are shareable artifacts.** A report is a self-contained HTML file —
  rendered in-app, exportable as-is to teammates who don't run the app.
- **Tool bindings are portable.** Starters bind Atlassian's official `acli`
  and `glab` — standard, individually authenticated CLIs any teammate can
  install — never personal wrapper scripts. Bindings stay profile-level
  (workflows-system.md §8), so swapping tools is config, not code.
- **Findings must carry evidence** (commits, MR links, file/line, grep output)
  or they don't render. Confidence is explicit.
- **Judgment is steerable per project** via a plain-markdown rulebook the user
  and the agent co-maintain (§5). Not a memory subsystem.
- **Every run shows its cost** (the usage ledger already attributes per
  work-item).

## 3. The reconciler (first automation, MVP)

A scheduled read-only workflow that diffs tracker state against git/MR reality
and produces one report of fingerprinted findings, each with an optional
one-keystroke action.

### Detection catalog

Ordered by determinism; MVP ships tiers A–B, tier C follows.

**Tier A — mechanical (cheap, near-certain):**

- `status-drift` — ticket In Progress/In Review with no branch, MR, or commit
  referencing its key within N days (default 14). Propose: transition to
  Backlog. *BLITZ evidence: BLITZ-9, 27, 31, 35–39, 108–110, 120, 121, 191.*
- `merged-not-closed` — a merged MR references the key but the ticket is open.
  Propose: transition to Done, with the MR link as receipt.
- `zombie-mr` — open MR with no update in N days (default 21) or whose diff has
  substantially landed via other commits. Propose: close MR, evidence attached.
  *BLITZ evidence: !35, !73, !88, !96, !100, !128, !175.*

**Tier B — cross-referencing (agent judgment, evidence-gated):**

- `already-fixed` — a backlog ticket's symptom matches code changed since the
  ticket was filed. High confidence → propose close with commit evidence;
  medium → report-only.
- `duplicate-cluster` — semantically overlapping open tickets. Propose:
  link-as-duplicate on the newer, keep the richer.

**Tier C — advisory (report-only, never proposes tracker writes):**

- `stale-solution` — the ticket's prescribed fix contradicts current code, or
  the real fix is deeper/adjacent. Renders as a sprint-planning aid. This is
  deliberately report-only: problems are real, prescriptions are notes.
- `attention-ranking` — aging In Review tickets and unreviewed MRs, ranked by
  staleness × conflict exposure (files touched vs. churn on main since branch
  point). "What needs a human first."

### The living findings ledger (incremental by construction)

A finding is `{kind, subject, evidence[], confidence, proposed_action?,
fingerprint, status, last_verified_at}` where the fingerprint is a stable hash
over kind + subject + salient facts, and status is one of **open / applied /
dismissed / resolved-by-reality / expired**. The ledger, not the run, is the
unit of truth; a run is a delta pass over it:

1. **Only changed subjects are re-investigated.** The run's cursor scopes the
   sweep: tickets with `updated >=` watermark, MRs updated since, commits to
   main since. Untouched subjects cost zero tokens.
2. **Open findings carry forward untouched** — past discoveries stay in the
   report as long as they remain relevant, without being re-derived.
3. **Findings whose subject changed get re-verified** (a targeted check, not a
   fresh investigation). Evidence gone → status flips to resolved-by-reality
   (ticket got moved, MR got closed, code got fixed) and the finding moves to
   the report's resolved section with what resolved it.
4. **Dismissals are deterministic.** A dismissed fingerprint is never re-raised
   unless its evidence materially changes (new fingerprint). Repeated
   dismissals of a pattern feed rulebook proposals (§5) — the agent asks to
   codify, it never silently learns.

### The report: agent-composed HTML over an app-owned component vocabulary

Each run emits two coupled outputs:

- **The findings envelope** (structured JSON, schema-validated) — updates the
  ledger; every finding carries its evidence chain and a machine-readable
  intended action.
- **The report document** — a self-contained HTML file the agent composes from
  the living ledger, rendered in-app (sandboxed webview) and exportable as-is:
  one file, no app required, bring-to-standup quality.

**Layout requirements (learned from Report #1):**

- **Priority-first, not kind-first.** The report leads with *what needs the
  human* (review queue, owner decisions), then cleanup ranked by impact;
  detection-kind grouping is secondary structure, not the spine.
- **Progressive disclosure is mandatory.** Every finding is one collapsed,
  scannable row — subject, one-liner, evidence-check count. Depth lives behind
  the expand. Nobody should have to read prose to find what they care about.
- **Trust comes from evidence transparency, not confidence labels.** Each
  finding renders *checked* vs. *not-checked* lists ("merged MR references
  ticket ✓ · no open MR ✓ · diff-vs-ticket-scope: not checked"). Similar
  findings share one investigation but each subject shows its own evidence
  row. Batch actions never hide per-item evidence.

**The custom-style / shared-functionality split.** Presentation is per-user
(rulebook-steered, agent-authored); interactive functionality is per-app and
must not be re-invented per report. The contract between them is a small
**component vocabulary** the app owns:

```
<ao-finding id>      row shell: collapse state, ledger binding, dismiss
<ao-subject>         ticket/MR reference → link, live status chip
<ao-evidence>        checked / not-checked chain, expandable
<ao-action>          intended action → "send to chat" + "copy brief"
```

The agent's HTML composes these elements freely — layout, grouping, ordering,
prose are its canvas and the user's taste. The in-app viewer registers the
implementations and a postMessage bridge; on export the app inlines static
fallbacks so links and copy-brief still work in a bare browser (composer
hand-off is in-app only). The envelope's finding ids bind DOM to ledger rows.

**Actions.** Primary: **send to chat** — pre-fills a new thread's composer
with the finding's brief (evidence + intended action), never auto-sent; the
thread's agent executes with the user's CLIs and the conversation is the
receipt. Secondary: **copy brief** — the same text for pasting into any chat.
Raw CLI command lines are not a user-facing surface. Resolution is confirmed
by the next delta run: reality changed → the finding moves to
*resolved-since-last-report* with what resolved it.

The HTML is agent-authored **so presentation is steerable the same way
judgment is**: report-layout and tone instructions live in the same per-project
rulebook (§5). "I don't like how this table reads" is a chat message → proposed
rulebook diff → next render improves. The composer is a refinable agent, not a
hardcoded template.

Run cost displays in the report card header (usage ledger, per work-item).

### What the MVP is

Nightly cron automation per project + on-demand "Run now". Tier A detections
only, the findings ledger with carry-forward and dismissal memory, the HTML
report over the component vocabulary, composer hand-off, rulebook injection
(§5), cost display. No poll triggers yet, no Tier B/C.

Acceptance test is live: run against BLITZ and it must propose (a) the ~11
relic In Progress transitions, (b) Done for merged-but-open tickets, (c) the 7
zombie MR closures — each with correct evidence — while raising nothing for the
9 genuinely active MRs.

## 4. Triggers (execution loop for §11)

The schema (`automations`, `automation_cursors`, migrations v23+) and store
CRUD exist; the missing piece is the scheduler loop. Per workflows-system.md
§11, unchanged:

- **Cron** — the reconciler's nightly sweep; any schedule.
- **Internal events** — item done/failed/needs-human; chains automations.
- **External sources** — polled through the user's authenticated CLIs by a
  query-and-enqueue workflow holding a cursor in `automation_cursors`. No
  webhooks, no stored credentials. First probes to ship as starters:
  `mr-created`, `mr-merged`, `ticket-created`, `ticket-transitioned`.

Desktop honesty: when the app is closed nothing polls; on boot, every
automation catches up from its cursor. That trade is also the moat — cloud
cron (e.g. Claude Code Routines) cannot run local authenticated CLIs against
local clones or render into local surfaces.

### Verified CLI surface (spike, 2026-07-14, live against BLITZ)

**Jira: Atlassian `acli` (OAuth via `acli jira auth login --web`).**

- `workitem search --jql <q> --json` — full issue objects; JQL supports
  `updated >= "<ts>"` windows and `--paginate`. **Constraint:** the JSON
  `fields` set is fixed (summary, status, assignee, issuetype, priority) —
  no `updated` timestamp in output, and `--fields` rejects it. Cursor design
  therefore: **watermark = poll wall-clock time minus a small overlap buffer**;
  the JQL window does the filtering, fingerprint dedup absorbs the overlap.
- `workitem view <key> --json` — description + status + transitions;
  `comment-list` for comments.
- Write verbs verified present: `transition --key --status`,
  `assign --key --assignee`, `link create` / `link type` (covers
  link-as-duplicate). All writes remain human-keystroke applies (§2).

**GitLab: `glab`** (already in daily use) — `mr list --output json` carries
`created_at` / `updated_at`, so MR probes cursor on real timestamps.

## 5. The rulebook (steerable judgment, not a memory system)

Per-project, plain markdown, size-capped, injected verbatim into automation
phase prompts:

```
<configRoot>/rules/<workflow-id>.md              # shared across projects
<configRoot>/projects/<slug>/rules/<workflow-id>.md   # project-specific, wins
```

The rulebook carries **both judgment and presentation**. Judgment: "OPS-*
branches are managed elsewhere — never flag their MRs." "BLITZ Epics are
containers; exclude from status-drift." "Anything touching `db/queries/cdc/`
is Mehrdad's — route attention findings to him." Presentation: "Lead with the
resolved section on Mondays." "Group zombie MRs by branch lineage, not age."
One steering loop for both: the report is wrong or ugly → say so in the
automation thread → proposed rulebook diff → one keystroke → next run improves.

Three write paths, one consent model:

1. **Direct edit** — it's a text file; the user owns it.
2. **Chat refinement** — the report card links to its automation thread; the
   user types "stop flagging X / treat Y as Z", the agent proposes a rule-file
   diff, one keystroke applies it.
3. **Dismissal patterns** — N dismissals of one kind/subject-class produce a
   proposed rule in the next report ("codify this?").

The agent never rewrites rules autonomously. Continuity across runs (what the
last sweep covered, watermarks in prose) lives in the existing per-automation
notes column — notes are state, rules are policy, and the two never mix.

## 6. Absorbing the MR tsunami (same primitives, next surfaces)

The trust chain for agent-implemented work is: **ticket correctness →
implementation → review → merge**. Today the first link is unchecked (tickets
aren't fully human-reviewed; some prescribe the wrong fix; some should be a
deeper refactor), which poisons everything downstream — and the last link rots
under conflict debt. Three surfaces, in dependency order:

### 6.1 Solution gate (pre-dispatch investigation)

Trigger: on demand, or `ticket-transitioned` into Selected. A read-only
workflow investigates the ticket against the current codebase and reports:
**agree** (prescription correct, here's the dispatch-ready spec + assumptions
to confirm) / **disagree** (evidence the prescription is wrong or stale) /
**deeper problem** (the real fix, scoped). Report in AO, never on the ticket.

The human reviews prose + evidence in minutes instead of discovering a
wrong-problem MR after hours. Only gated tickets become eligible for agent
implementation — this encodes the stated trust boundary: "if the ask is clear,
correct, and the best solution, agent workflows actually work; my job is
reviewing assumptions and decisions."

### 6.2 Review packet (per-MR reviewer compression)

Trigger: `mr-created` / `mr-updated`. A read-only workflow produces, per MR:
claims→hunks mapping (what the ticket/description promises, where the diff
delivers it), an **assumptions and decisions ledger** front and center, gate
receipts, blast radius, and a risk-ranked reading order. Rendered beside the
diff (prthread/ReviewPane surfaces).

This attacks the binding constraint directly: reviewer minutes per MR. It does
not and must not replace review — it removes the archaeology from it. It also
halves the cross-review problem (two heavies reviewing each other's agent-speed
output) and is the single best new-hire ramp artifact: every MR arrives with
its own "what/why/where to look".

### 6.3 Landing train (conflict-debt liquidation)

Trigger: on demand ("land this set") or `mr-merged` (advance the train). The
workflow computes the file-overlap graph across open MRs, proposes a landing
order that minimizes conflicts, then serially: rebase the next MR onto current
main, re-run its gates, and present it review-ready. Mechanical conflicts are
resolved and disclosed (the resolution diff is part of the packet); semantic
conflicts stop the train and surface as a needs-human finding.

This converts "a week of fighting conflicts" into a supervised sequence where
the human only ever reviews an MR that is already green against today's main.
Rebases are within the trust boundary because gates re-run and the resolution
diff is itself reviewed.

### 6.4 The reference-branch pattern (queue semantics change)

For agent-implemented work that cannot land promptly, stop treating the MR as
the durable artifact. The durable artifact is **spec + assumptions ledger +
reference diff on a throwaway branch**. When the item's landing slot arrives,
re-derive the change on current main from the spec (fresh, conflict-free by
construction) and review the fresh diff with the reference as baseline —
"what changed vs. the reference" is a small, answerable question. This trades
tokens for conflict debt, which is exactly the available currency. The landing
train (6.3) manages live MRs that already exist; this pattern prevents the
queue from minting new rot.

## 7. Engine mapping

| Need | Exists | Missing |
|---|---|---|
| Run records, phases, receipts, digests | `work_items*` (v23–v26) | — |
| Automation definitions + cursors + notes | `automations.go`, `automation_cursors`, notes RPCs | scheduler loop (cron tick, internal-event fan-in, poll dispatch) |
| Read-only runs without worktrees | `DeriveWorkspaceNeed` → project-root workspace | read-only enforcement posture per phase |
| Dynamic/composed definitions | `InlinePrompts` | — |
| Per-run cost | `usage_ledger.work_item_id` | surface on report card |
| Report surfaces | M4 UI (fold, done-row, triage threads); composer drafts (`composerdraft`) | component registry + sandboxed report viewer + postMessage bridge; send-to-composer hand-off; export with static fallbacks |
| Findings persistence | — | new table: the living ledger (fingerprint, status, last_verified_at) |
| Rulebook | profile/config dir conventions | rules resolution + prompt injection + proposed-diff apply flow |
| CLI access | `acli` + `glab` verified live (§4); profile-bound commands | starter probe workflows |
| Mediated read-only tracker/forge tools | design-mode MCP + enqueue-tool precedent | automation tool surface (`tracker_search`, `mr_view`, …) |

Deletion candidates once this lands: the studio-thread stub as an entry point
(`app_workflow_studio.go` in its current form), the stale triage framing
(`app_workflow_triage.go:27`), and the definition-catalog-first UI emphasis —
the catalog becomes plumbing behind automations, not a destination.

## 8. Execution contract & credential posture

### Where phases run

Engine-spawned provider sessions — the same managed Claude/Codex CLIs the app
already runs — with workspace = project root under the read-only workspace
need; no worktree. Read-only is enforced with provider-native postures per
phase (Codex `-s read-only`; Claude with a write-excluded toolset), not prose.

### What a run sees (context composition, every part bounded)

1. **Base prompt** — the workflow's prompt `.md` (starter-installed,
   user-editable; this is the "refinable agent").
2. **Rulebook overlay** (§5), size-capped.
3. **Continuity notes** (existing automation notes column).
4. **Pre-fetched input data** — the app runs the deterministic queries
   agent-free (acli JQL window, `glab mr list`, `git log` range) and passes
   the results as input payloads. The agent receives data, not credentials.
5. **Ledger snapshot** — open findings (fingerprint + one-line summary) for
   carry-forward and re-verify targeting.
6. **The findings envelope schema** (existing `def` envelope machinery).

### Tools and credentials

CLI-side permission scoping is mostly unavailable: acli documents token auth
as "API token without scopes" and its OAuth grant is fixed; `glab`/`gh` OAuth
grants are broad. GitLab PATs (`read_api`) and GitHub fine-grained PATs are
real read-only fences injectable via `GITLAB_TOKEN`/`GH_TOKEN`. The design
does not lean on any of it:

- **Agent phases hold zero tracker/forge credentials.** Ad-hoc queries go
  through mediated read-only tools served by the app's MCP surface (precedent:
  design-mode tools, the workflow enqueue tool): `tracker_search`,
  `tracker_view`, `mr_list`, `mr_view`. Each is implemented app-side by
  shelling the profile-bound CLI under the user's auth; only read verbs exist
  on the tool surface, so the credential never enters the agent process.
- **Writes only happen in an interactive chat thread the user explicitly
  sends** (the composer hand-off). Automation phases cannot transition a
  ticket or close an MR — nothing in their environment can — and the app
  itself holds no write path either.
- Belt-and-braces where supported: if a phase ever needs direct CLI access,
  inject platform read-only tokens (`GITLAB_TOKEN` with `read_api`, GitHub
  fine-grained). Atlassian offers no equivalent — one more reason acli stays
  app-side only.

### Budgets

Per-run token budget via the existing work-item usage attribution
(`QueryWorkItemUsage`); a run that hits budget parks with a typed reason
instead of degrading its output silently.

## 9. Sequencing

1. **Scheduler loop + reconciler MVP (Tier A) + findings ledger + component
   vocabulary + composer hand-off + rulebook injection.** Acceptance: the live
   BLITZ run in §3.
2. **Rule-proposal flow + Tier B detections.**
3. **Poll probes (`mr-*`, `ticket-*`) + review packet (6.2).**
4. **Solution gate (6.1), landing train (6.3), Tier C advisory findings.**

Each step is independently useful; nothing depends on a later step.

## 10. Open questions

- **Acting on teammates' artifacts:** should the reconciler propose closing a
  zombie MR authored by someone else, or render it FYI-only with a "nudge"
  action (draft a message to the author) instead? Default until decided:
  propose freely on your own artifacts, FYI-only on others'.
- **Report cadence vs. noise:** nightly per-project card, or a single morning
  digest across projects? Default: per-project card, digest later.
- **Solution-gate placement:** gate on Selected-for-Development transition, or
  purely on demand? Default: on demand first; transitions once poll probes ship.
- **Reference-branch re-derivation:** always re-derive, or attempt rebase first
  and re-derive only past a conflict threshold? Default: rebase first, re-derive
  on threshold — cheaper, same review posture.
