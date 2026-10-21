<script lang="ts">
  // The Workflows tool pane (UI-SPEC §2.1): the `workflows` pane kind, mounted
  // by panes/ToolPane.svelte. The pane exists exactly while Workflows is open;
  // the stack, project filter and sweep cursor live in the store so they
  // survive the pane closing and a restart.
  //
  // Two levels plus a terminal one — home › run detail, and all-clear.

  import { onDestroy, untrack } from 'svelte';
  import ToolPaneHeader from '../panes/ToolPaneHeader.svelte';
  import WorkflowsHome from './WorkflowsHome.svelte';
  import WorkflowRunDetail from './WorkflowRunDetail.svelte';
  import WorkflowAllClear from './WorkflowAllClear.svelte';
  import WorkflowIntakeDialog from './WorkflowIntakeDialog.svelte';
  import WorkflowDiscardDialog from './WorkflowDiscardDialog.svelte';
  import { getProjects } from '../../stores/projects.svelte';
  import {
    clearWorkflowReceipts,
    getWorkflowRun,
    isWorkflowOverlayLoaded,
    isWorkflowLoading,
    getWorkflowLoadError,
    loadWorkflowsOverlayData,
    retainWorkflowDetails,
  } from '../../stores/workflowRuns.svelte';
  import {
    getWorkflowsOverlayDialog,
    getWorkflowsOverlayTop,
    popWorkflowsOverlay,
    pruneWorkflowsOverlayStack,
    setWorkflowsOverlayDialog,
  } from '../../stores/workflowsOverlay.svelte';
  import { cancelWorkflowAutoAdvance } from '../../stores/workflowResolve';
  import { setWorkflowsScroller } from './workflowsScroller';

  interface Props {
    paneId: string;
    onPaneDragStart?: (event: DragEvent) => void;
  }
  let { paneId, onPaneDragStart }: Props = $props();

  let top = $derived(getWorkflowsOverlayTop());
  let runId = $derived(top.level === 'run' ? top.itemId : '');
  let run = $derived(runId ? getWorkflowRun(runId) : undefined);
  let dialog = $derived(getWorkflowsOverlayDialog());

  let bodyEl = $state<HTMLElement | null>(null);
  let levelKey = $derived(`${top.level}:${runId}`);

  // RUN-MAP §9.9 — this frame owns the one scroller, so it says so rather than
  // leaving the run map to find it by walking the DOM. A getter, because the
  // binding lands after this runs.
  setWorkflowsScroller(() => bodyEl);

  // RUN-MAP §9.9 — one scroller serves every level, so where a level swap
  // leaves the reader is this component's contract, not an emergent property.
  //
  // Two other things happen to land the same answer: swapping the `{#if}`
  // branch below empties the scroller for a moment, which makes the browser
  // clamp `scrollTop` to 0, and the run map's `placeOnOpen` writes 0 for a
  // parked or terminal run. Neither is a promise. The first is Svelte's
  // insertion order plus a browser clamp; the second only exists on levels that
  // mount a map, and only once its view has landed. A stated scroll contract
  // that depends on transient DOM emptiness is one refactor from breaking with
  // nothing to catch it, so it is stated here instead.
  //
  // A PRE effect, and only a pre effect: it runs before the new level's DOM
  // exists, so the reset is never a visible jump, and it is strictly earlier
  // than the map's mount-time placement (§9.5), which must have the last word
  // about where a RUNNING run opens. `bodyEl` is read untracked because the
  // reset is caused by NAVIGATION, never by the element binding.
  $effect.pre(() => {
    void levelKey;
    untrack(() => {
      if (bodyEl !== null) bodyEl.scrollTop = 0;
    });
  });

  // Project MEMBERSHIP as a $derived key: the projects array is replaced
  // wholesale by every activity touch (any thread's turn-completed bumps
  // its project's lastActive), so an effect reading the array re-hydrated
  // the whole surface on a per-turn cadence while it was open. The key
  // changes only when a project is added or removed — which is when a
  // re-list is actually wanted. Load order is irrelevant downstream (the
  // ids land in a Set).
  let projectIdsKey = $derived(
    getProjects()
      .map((entry) => entry.project.id)
      .join('\u0000'),
  );

  // Hydrate once per mount. A reopened pane re-lists so a run started from the
  // CLI while it was closed is present, but keeps the cached catalogs.
  $effect(() => {
    const projectIds = projectIdsKey.length === 0 ? [] : projectIdsKey.split('\u0000');
    void loadWorkflowsOverlayData(projectIds).then(() => {
      // Restored stack entries whose run no longer exists drop from the top
      // (§2.1); home is always valid.
      pruneWorkflowsOverlayStack((itemId) => getWorkflowRun(itemId) !== undefined);
    });
  });

  // Frontend memory stays bounded by what is on screen: leaving a run's detail
  // drops its (and its children's) phases, units and artifacts.
  $effect(() => {
    retainWorkflowDetails(runId || null);
  });

  // Session receipts exist to hold a resolved run in the sweep long enough to
  // read its receipt. Closing the pane ends that session — and cancels any
  // pending auto-advance, so a step never lands on a surface nobody is on.
  onDestroy(() => {
    clearWorkflowReceipts();
    cancelWorkflowAutoAdvance();
  });
</script>

<!--
  Esc is bound globally (`workflows.escape`, §8) with the precedence the stack
  needs, so the pane carries no key handler of its own — a local one would
  consume the same press twice.
-->
<div class="flex h-full min-h-0 flex-col" data-testid="workflows-pane">
  <ToolPaneHeader
    {paneId}
    kind="workflows"
    {onPaneDragStart}
    onBack={top.level === 'home' ? undefined : () => { popWorkflowsOverlay(); }}
    backLabel="Back (esc)"
    backTestId="workflows-back"
  >
    {#if top.level !== 'home'}
      <nav class="min-w-0 truncate text-sm text-fg-muted" data-testid="workflows-breadcrumb">
        <span class="pr-1.5 text-fg-subtle">›</span><span class="text-fg">{top.level === 'all-clear' ? 'All clear' : (run?.goal || 'Run')}</span>
      </nav>
    {/if}
  </ToolPaneHeader>

  <!--
    `overflow-anchor:none` because the run map owns its own compensation
    (RUN-MAP §9.7): native scroll anchoring picks its own anchor element and
    fights the anchor-hold, which is the difference between a fold that holds
    still and one that fights you.
  -->
  <div class="min-h-0 flex-1 overflow-y-auto [overflow-anchor:none]" bind:this={bodyEl} data-testid="workflows-pane-body">
    {#if getWorkflowLoadError() && !isWorkflowOverlayLoaded()}
      <p class="px-4 py-6 text-xs text-error" data-testid="workflows-load-error">{getWorkflowLoadError()}</p>
    {:else if !isWorkflowOverlayLoaded() && isWorkflowLoading()}
      <p class="px-4 py-6 text-xs text-fg-muted">Loading workflows…</p>
    {:else if top.level === 'home'}
      <WorkflowsHome />
    {:else if top.level === 'run'}
      <WorkflowRunDetail itemId={runId} />
    {:else}
      <WorkflowAllClear />
    {/if}
  </div>
</div>

<WorkflowIntakeDialog
  open={dialog === 'intake'}
  onClose={() => setWorkflowsOverlayDialog(null)}
/>
<WorkflowDiscardDialog
  open={dialog === 'discard'}
  itemId={runId}
  onClose={() => setWorkflowsOverlayDialog(null)}
/>
