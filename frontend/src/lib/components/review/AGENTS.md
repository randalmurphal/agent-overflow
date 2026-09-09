# components/review/

This directory renders workspace diffs, pull or merge request detail, review
threads, CI state, and review actions. Data remains owned by the workspace and
PR stores; components own presentation and interaction.

## Identity and routing

- Resolve workspace operations from the pane's `WorkspaceRef`, including draft
  panes without a thread row.
- PR data is keyed by computer and repository identity. Route forge reads and
  mutations to that computer rather than current global focus.
- A loaded diff records the head SHA it represents in pane-local state. Derive
  staleness against the store's live head so two panes can hold different
  snapshots safely.
- Capture mutation targets before awaiting. Late replies update the captured
  PR or thread and must not navigate a pane whose target changed.

## Rendering contracts

- Keep raw patch and comment content canonical. Derive file rows, hunks,
  comments, syntax spans, and display metadata locally.
- Virtualized review lists use the generic adapter in
  [`components/virtual/`](../virtual/AGENTS.md). Review-specific anchoring and
  selection stay here.
- Preserve line identity across refreshes so open threads, selections, and
  measured rows do not move unnecessarily.
- Embedded forge HTML uses `ChatMarkdown`'s opt-in sanitized mode. Do not render
  arbitrary HTML or bypass URL transformation.
- Collapse and load large files explicitly. Avoid mounting hidden diff bodies or
  preloading every payload.
- Wrap the review surface in `shared/RenderBoundary.svelte`.

Review mutations reconcile from authoritative responses and show partial or
failed outcomes in the affected surface. Never optimistically erase unresolved
threads, failed comments, or stale-head warnings.

Test pure diff projection separately from component interaction. Use browser
tests for virtualized geometry, selection, sticky headers, and scroll anchoring.
