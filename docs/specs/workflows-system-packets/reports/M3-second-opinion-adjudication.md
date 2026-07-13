# M3 second-opinion review — adjudication

Sources: 3 codex read-only reviews (gpt-5.6-sol, xhigh; session ids in LEDGER) over
stores (S#), components (C#), backend (B#); 4 fable UI-SPEC conformance audits
F1 (§§2–4), F2 (§5+§9), F3 (§§6/7/11), F4 (§§8/10/12). Every finding below was
verified against the code in this tree before adjudication; reports were treated
as claims, not evidence.

Verdicts: CONFIRMED (defect verified), DESIGN (works as designed / needs no fix),
REFUTED, DEFER (assigned to a later pass or milestone).

## Rulings (spec adjudications made here)

- **R-A Failed re-enqueue** (§5.3 failed row, WHAT-spec §"Resolving at scale"):
  "Re-enqueue with guidance" means failed → **queued** with the latest
  diagnosis primed as feedback — an engine lifecycle action alongside
  `parkDisposition`, not a widened `resume`. Target phase = latest attempt's
  phase, attempt reset, queue tail position.
- **R-B Disposition parks**: `WorkflowMergeItem` / `WorkflowCreateItemPR` must
  also accept `needs-human(disposition)` (a merge refusal is currently a dead
  end: merge demands `done`). Success on a parked item unparks
  needs-human(disposition) → done via a companion lifecycle action.
  `engine.resume` must reject `ReasonDisposition`. UI: disposition-parked runs
  render the done-manual action row (Merge / Create PR / Discard / Continue),
  not "Re-enqueue with guidance".
- **R-C Done hand-off**: `WorkflowOpenTriageThread` accepts `done` (worktree
  still present pre-discard) per §8.2/§5.3.
- **R-D Discard**: discard is the destructive dismissal affordance; it must
  work for read-only workflows (no worktree by design), setup-failed and
  pre-provision parks (record-only receipt), and dirty worktrees (the armed
  double-press is the authorization). The worktree registration check stays;
  the dirty check and the missing-worktree error do not apply to discard.
- **R-E Chat proposal flow (§7.2)**: no producer exists in M3 (grep-verified);
  `WorkflowConfirmCard` was built ahead of its emitter. DEFER to M4 with a spec
  annotation; do not mount a card nothing can produce.
- **R-F Merge locality (B8)**: `MergeBranch` staying local-branch-only is the
  intended local-first behavior. No upstream-remote verification in M3.
- **R-G Takeover worktree (B6)**: REFUTED — preserving the worktree across
  takeover/resume is the documented engine invariant; resetting would destroy
  the human's edits.
- **R-H Transport proxy (B1)**: real, pre-dates M3, applies to the entire
  `LocalOnlyMethods` surface. Handled as a docs caveat + future hardening item,
  not a workflows fix.

## Fix pass A — backend (packet P3.8)

| ID | Verdict | Fix |
|---|---|---|
| C5=B7=F4.1 | CONFIRMED | app_workflow_triage.go:39 accepts done (R-C) + test |
| B4=F2.1 | CONFIRMED | Failed re-enqueue lifecycle action per R-A; wire as `WorkflowResumeItem` alias or new RPC + tests |
| B5 | CONFIRMED | resume rejects ReasonDisposition; merge/PR accept needs-human(disposition); unpark-on-success (R-B) |
| B9 | CONFIRMED | Thread base branch through Forge.CreatePR (gh `--base`, glab `--target-branch`, nullForge, tests); disposition passes profile/item base; receipt gains `base` field |
| B10 | CONFIRMED | Landed disposition must not surface as RPC error when only auto-cleanup failed: receipt carries `cleanupFailed`; error goes to workflow:error |
| B11 | CONFIRMED | Startup rebuild sweeps orphan items of deleted projects (app-wide list, mark cancelled/interrupted) + test |
| C6 | CONFIRMED | Discard per R-D: tolerate missing worktree, allow dirty worktree, keep registration check + tests |
| S12 (BE half) | CONFIRMED | (FE filters null; BE unchanged) |
| F3.2 | CONFIRMED | Base branch override at intake: `WorkflowEnqueueItem` gains optional base param (WHAT-spec: "base overridable at intake"); bindings regen |
| F4.6 | CONFIRMED | Show/Restore/Focus on notification activation (desktop + Windows launcher) |
| B3 | DESIGN | Release-on-failed-Stop is the lesser evil (holding leaks forever); error already joins upward. No change; noted. |
| B8 | DESIGN (R-F) | none |
| B6 | REFUTED (R-G) | none |
| B1 | DEFER (R-H) | transport AGENTS.md caveat line lands with P3.8 docs |
| B2 | DEFER→perf | WorkflowGetItem payload trim evaluated in perf pass (snapshot is a live UI dependency today) |

## Fix pass B — frontend + e2e (packet P3.9)

| ID | Verdict | Fix |
|---|---|---|
| S1 | CONFIRMED | loadWorkflowCurrentLevel re-checks current level after awaits |
| S2+S3 | CONFIRMED | Pane adopts sidebar pattern: coalesced authoritative refresh on item events + capture-events-during-fetch |
| S4 | CONFIRMED | applyTransportGap gains workflow:* case → refresh sidebar + pane level |
| S6=F1.4 | CONFIRMED | Retarget stops mutating projectFilter; target loads regardless of filter |
| S7=C2 | CONFIRMED | stepWorkflowSweep clears autoAdvanceTimer |
| S8 | CONFIRMED | sweepIndex clamped/recomputed when the sweep set changes |
| S9 | CONFIRMED | Restore truncates on missing targets at any level; robust not-found match |
| S10 | CONFIRMED | Post-hydration activations serialize through the same queue |
| S5 | CONFIRMED | Sidebar definitions fetched for definition-bearing projects, not only run-bearing ones |
| C10 | CONFIRMED | Intake loads definitions for the selected project directly, not pane-filtered store |
| C1 | CONFIRMED | Armed-action key includes state+reason |
| C3 | CONFIRMED | Session receipt renders alongside (not instead of) persisted disposition block |
| C4=F2.13 | CONFIRMED | handleActionKey short-circuits when receipt/disposition present |
| C7/C8=F1.7/F1.8 (partial) | CONFIRMED | Cross-project drag gets feedback (toast) instead of silent no-op; after-last drop zone + drop indicator. Global cross-project ordering itself: DEFER (design change, flagged) |
| C9 | CONFIRMED | In-flight guards on overview cancelQueued (and reorder) |
| F1.1 | CONFIRMED | Run rows render "Needs you" for needs-human |
| F1.2 | CONFIRMED | Run-row meta = human fragments (reason word, parked age, phase progress, question snippet) — R2 |
| F1.3 | CONFIRMED | Done-awaiting-disposition listed under Runs (isWorkflowParked), History after disposal |
| F3.6 | CONFIRMED | Sweep-at-run entry with empty sweep set → all-clear |
| F2.3 | CONFIRMED | Neutral state words capitalized via workflowRunSignal mapping (+2 e2e assertions) |
| F2.14 | CONFIRMED | Reject note input autofocused |
| F2.16 | CONFIRMED | Question quote + digit suggestions sourced from the parked phase envelope, not the rewritable digest |
| F2.19=F4.8 | CONFIRMED | "run's worktree" |
| S11 | DEFER→perf | server-side States filter + summary counts |
| C11=F2.15=F4.7 | CONFIRMED | Artifact rows disabled remotely ("Local only"); open-by-type deferred to polish |
| C12 | CONFIRMED | e2e additions: failed re-enqueue, done continue-with-agent, disposition-parked merge retry, receipt keyboard guard |
| C13 | CONFIRMED (test-quality) | companion e2e ties companion to the expected phase thread |
| F4.9 | CONFIRMED→polish | reorder grip disabled-with-tooltip |

## Polish pass (task #5) — deferred copy/layout batch

F2.4 (sweep dots + membership), F2.5 (header row3 phase/parked/automation),
F2.6 (Approve → next phase), F2.7 (merge receipt mode label + actual base name +
worktree-cleaned), F2.8 (auto-merge policy copy + undo row), F2.9 (failed
evidence: humanized reason, failing check ×N, diagnosis quote), F2.10 (gate
auto-loads diff file list), F2.11 (queued rank not sortPosition), F2.12
(automation re-proposal note), F2.17=F1.9 (Backspace pop-only), F2.18 (all-clear
kind labels), F2.15 (named outputs kv + open-by-type), F1.5 (idle/last-run
aggregate), F1.6=F3.3 (human-gate count on wire + line 2), F1.7 remainder
(project dot, workflow name, queued age), F1.10 (container queries + meta
truncation), F1.11 (§2.2 vs §6.1 header reconcile), F1.12 (global slots on
queue-state event), F1.13 (controls as header chrome), F1.14 (receipt order),
F3.4 (multiline seed signal), F3.5 (path picker files), F3.7 (phase in summary
for cold-load progress), F3.8 (hint tone), F4.9.

## Flagged to user (scope decisions, not silently deferred)

- F2.2: §5.3 done/PR "Review comments (N) / Send comments to the agent" flow
  (D11) is unimplemented — feature-sized, needs its own packet.
- F3.1/R-E: §7.2 chat proposal producer absent — M4 annotation.
- F4.3: triage-agent spawn tools — D10 sequences to M4.
- F4.5: drain summary is per-project (structurally forced) — ratify in §10.
- F4.4: triage seed carries diff summary, not diff — ratify or size-capped diff.
- F4.2: triage seed renders as raw first message (R2) — needs a context-chip
  timeline treatment; UI-feature-sized, grouped with F2.2 scope talk.
- C7/C8 remainder: truly global queue ordering is a schema/engine design change.
- B1/R-H: same-host reverse proxy defeats loopback locality app-wide.
