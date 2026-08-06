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

  import { onDestroy } from 'svelte';
  import ChevronLeft from 'lucide-svelte/icons/chevron-left';
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

  interface Props { open: boolean }
  let { open }: Props = $props();

  let top = $derived(getWorkflowsOverlayTop());
  let runId = $derived(top.level === 'run' ? top.itemId : '');
  let run = $derived(runId ? getWorkflowRun(runId) : undefined);
  let dialog = $derived(getWorkflowsOverlayDialog());

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

  <div class="min-h-0 flex-1 overflow-y-auto">
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
