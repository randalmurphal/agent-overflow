<script lang="ts">
  // The workflows overlay frame (UI-SPEC §2.1). Mounted in App.svelte as a
  // SIBLING of <PaneHost>, layered above it — never passed into PaneHost,
  // never a pane kind. The pane tree stays mounted and untouched underneath,
  // so closing is a pure unmount of this layer: zero pane rebuild, zero
  // virtualizer resync.
  //
  // Two levels plus a terminal one — home › run detail, and all-clear. The
  // stack, project filter and sweep cursor live in the store so they survive
  // close/reopen and restart.

  import { onDestroy, untrack } from 'svelte';
  import ChevronLeft from '@lucide/svelte/icons/chevron-left';
  import Icon from '../primitives/Icon.svelte';
  import IconButton from '../primitives/IconButton.svelte';
  import OverlayShell from '../primitives/OverlayShell.svelte';
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
    closeWorkflowsOverlay,
    getWorkflowsOverlayDialog,
    getWorkflowsOverlayTop,
    popWorkflowsOverlay,
    pruneWorkflowsOverlayStack,
    setWorkflowsOverlayDialog,
  } from '../../stores/workflowsOverlay.svelte';
  import { cancelWorkflowAutoAdvance } from '../../stores/workflowResolve';
  import { setWorkflowsOverlayScroller } from './overlayScroller';

  interface Props { open: boolean }
  let { open }: Props = $props();

  let top = $derived(getWorkflowsOverlayTop());
  let runId = $derived(top.level === 'run' ? top.itemId : '');
  let run = $derived(runId ? getWorkflowRun(runId) : undefined);
  let dialog = $derived(getWorkflowsOverlayDialog());

  let bodyEl = $state<HTMLElement | null>(null);
  let levelKey = $derived(`${top.level}:${runId}`);

  // RUN-MAP §9.9 — this frame owns the one scroller, so it says so rather than
  // leaving the run map to find it by walking the DOM. A getter, because the
  // binding lands after this runs.
  setWorkflowsOverlayScroller(() => bodyEl);

  // RUN-MAP §9.9 — one scroller serves every level, so where a level swap
  // leaves the reader is this component's contract, not an emergent property.
  //
  // Today two other things happen to land the same answer: swapping the `{#if}`
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

  // Hydrate once per open. A reopen re-lists so a run started from the CLI
  // while the overlay was closed is present, but keeps the cached catalogs.
  $effect(() => {
    if (!open) return;
    const projectIds = getProjects().map((entry) => entry.project.id);
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
  // read its receipt. Closing the overlay ends that session — and cancels any
  // pending auto-advance, so a step never lands on a surface nobody is on.
  function endSession(): void {
    clearWorkflowReceipts();
    cancelWorkflowAutoAdvance();
  }

  $effect(() => {
    if (!open) endSession();
  });

  onDestroy(endSession);
</script>

<!--
  Esc is bound globally (`workflows.escape`, §8) with the precedence the stack
  needs, so the shell deliberately carries no key handler of its own — a local
  one would consume the same press twice.
-->
<OverlayShell
  {open}
  ariaLabel="Workflows"
  onScrimClick={closeWorkflowsOverlay}
  scrimTestId="workflows-overlay-scrim"
  testId="workflows-overlay"
>
  <header class="flex shrink-0 items-center gap-2 border-b border-border-subtle px-4 py-3">
    {#if top.level === 'home'}
      <h2 class="text-sm font-semibold text-fg">Workflows</h2>
    {:else}
      <IconButton
        size="sm"
        label="Back (esc)"
        testId="workflows-back"
        onClick={() => { if (!popWorkflowsOverlay()) closeWorkflowsOverlay(); }}
      >
        <Icon icon={ChevronLeft} size={14} strokeWidth={2} />
      </IconButton>
      <nav class="min-w-0 flex-1 truncate text-sm text-fg-muted" data-testid="workflows-breadcrumb">
        Workflows<span class="px-1.5 text-fg-subtle">›</span>
        <span class="text-fg">{top.level === 'all-clear' ? 'All clear' : (run?.goal || 'Run')}</span>
      </nav>
    {/if}
  </header>

  <!--
    `overflow-anchor:none` because the run map owns its own compensation
    (RUN-MAP §9.7): native scroll anchoring picks its own anchor element and
    fights the anchor-hold, which is the difference between a fold that holds
    still and one that fights you.
  -->
  <div class="min-h-0 flex-1 overflow-y-auto [overflow-anchor:none]" bind:this={bodyEl} data-testid="workflows-overlay-body">
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
</OverlayShell>

{#if open}
  <WorkflowIntakeDialog
    open={dialog === 'intake'}
    onClose={() => setWorkflowsOverlayDialog(null)}
  />
  <WorkflowDiscardDialog
    open={dialog === 'discard'}
    itemId={runId}
    onClose={() => setWorkflowsOverlayDialog(null)}
  />
{/if}
