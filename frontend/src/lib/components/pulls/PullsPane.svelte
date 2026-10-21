<script lang="ts">
  import { onDestroy, untrack } from 'svelte';
  import RefreshCw from '@lucide/svelte/icons/refresh-cw';
  import Sparkles from '@lucide/svelte/icons/sparkles';
  import ChevronDown from '@lucide/svelte/icons/chevron-down';
  import ToolPaneHeader from '../panes/ToolPaneHeader.svelte';
  import Button from '../primitives/Button.svelte';
  import Icon from '../primitives/Icon.svelte';
  import MenuItem from '../primitives/MenuItem.svelte';
  import PaneHeaderIconButton from '../panes/PaneHeaderIconButton.svelte';
  import Segmented from '../primitives/Segmented.svelte';
  import ReviewPane from '../review/ReviewPane.svelte';
  import OpenWithAgentMenu from './OpenWithAgentMenu.svelte';
  import PipelineRow from './PipelineRow.svelte';
  import PipelineView from './PipelineView.svelte';
  import PipelinesFilterChips from './PipelinesFilterChips.svelte';
  import PullRow from './PullRow.svelte';
  import PullsFilterChips from './PullsFilterChips.svelte';
  import PullsGroupHeader from './PullsGroupHeader.svelte';
  import PullsProjectError from './PullsProjectError.svelte';
  import { restorePickerFocus } from '../panes/paneComposerFocus';
  import { pullGroupLabel } from '../../utils/pullsAttention';
  import { copyToClipboard } from '../../utils/clipboard';
  import { errString } from '../../utils/errors';
  import { forgeLabels } from '../../utils/forgeLabels';
  import type { PopoverCloseReason } from '../../utils/popoverOwnership';
  import { appStorageGet, appStorageSet } from '../../stores/appStorage';
  import { OpenExternalURL } from '../../stores/bindings';
  import { isCompactLayout } from '../../stores/layoutMode.svelte';
  import {
    attachPipelinesPage,
    clearPipelineSelection,
    detachPipelinesPage,
    pipelineDetailOpen,
    pipelinesError,
    pipelinesLoading,
    pipelinesProjectErrors,
    pipelinesProjects,
    pipelinesReading,
    pipelinesRows,
    refreshPipelinesPage,
    selectPipeline,
    selectedPipeline,
    setPipelinesFilters,
    setVisiblePipelines,
  } from '../../stores/pipelinesPage.svelte';
  import { registerPullsBackHandler } from '../../stores/pullsCommands.svelte';
  import {
    attachPullsPage,
    clearSelection,
    detachPullsPage,
    detailOpen,
    pullsError,
    pullsLoading,
    pullsNeedsYouCount,
    pullsProjectErrors,
    pullsProjects,
    pullsRefreshing,
    pullsRowsByGroup,
    pullsSnapshotError,
    refreshPullsPage,
    selectPR,
    selectedPR,
    setFocusedRows,
  } from '../../stores/pullsPage.svelte';
  import {
    loadThreadsOnPR,
    newThreadCheckedOutBy,
    openPRThread,
    openThread,
    projectThreads,
    sendDrafts,
    threadsOnPR,
    threadsOnPRLoading,
  } from '../../stores/pullsPageAgent.svelte';
  import { mrFilters, pipelineFilters, setMRFilters } from '../../stores/pullsPageFilters.svelte';
  import { currentPinnedReviewState } from '../../stores/reviewPane.svelte';
  import { selectedBackend } from '../../stores/selectedBackend.svelte';
  import { addErrorToast } from '../../stores/toast.svelte';
  import { registerToolDetailProbe } from '../../stores/toolPanes.svelte';
  import type { PullRowView } from './pullRowView';
  import type { PipelineRowView } from './pipelineRowView';
  import { visibleRows } from './visibleRows';

  // The Pull requests tool pane (docs/specs/pull-requests.md, "The page"):
  // a list column with two tabs, Merge requests (the filter chips, the
  // Needs you and Waiting on others groups, each project's failure under
  // its own heading) and Pipelines (project, ref and status filters over
  // one row per pipeline), beside the detail: the review pane pinned to
  // the picked MR, or the picked pipeline's stages and logs. The list
  // column is resizable and remembered; compact shows the list as the
  // page and the detail full width with Back. The page lists the computer
  // selected when it opened and keeps it for the pane's life.

  interface Props {
    paneId: string;
    onPaneDragStart?: (event: DragEvent) => void;
  }

  let { paneId, onPaneDragStart }: Props = $props();

  type Tab = 'mrs' | 'pipelines';
  const TAB_KEY = 'pulls:tab';
  const LIST_WIDTH_KEY = 'pullsListWidth';
  const LIST_MIN_PX = 280;
  const LIST_MAX_PX = 720;
  const LIST_DEFAULT_PX = 400;

  const backend = untrack(() => selectedBackend());
  let tab: Tab = $state(appStorageGet(TAB_KEY) === 'pipelines' ? 'pipelines' : 'mrs');
  let compact = $derived(isCompactLayout());
  let listWidth = $state(readListWidth());

  const selection = $derived(selectedPR(paneId));
  const pipeline = $derived(selectedPipeline(paneId));
  const detailShown = $derived(tab === 'mrs' ? selection !== null : pipeline !== null);
  const filters = $derived(mrFilters(backend));
  const groups = $derived(pullsRowsByGroup(paneId));
  const projectErrors = $derived(pullsProjectErrors(paneId));
  const rowCount = $derived(groups.reduce((sum, group) => sum + group.rows.length, 0));
  const pipeFilters = $derived(pipelineFilters(backend));
  const pipeRows = $derived(pipelinesRows(paneId));

  // The page lists the computer it opened on until the pane goes. The
  // pane id is fixed for a tool pane's life (`tool-pulls`), so the initial
  // value is the only value.
  // svelte-ignore state_referenced_locally
  const ownerPaneId = paneId;
  attachPullsPage(ownerPaneId, backend);
  $effect(() => {
    if (tab === 'pipelines') {
      attachPipelinesPage(paneId, backend);
      return () => detachPipelinesPage(paneId);
    }
  });
  const disposeProbe = registerToolDetailProbe('pulls', () => (tab === 'mrs' ? detailOpen(paneId) : pipelineDetailOpen(paneId)));
  const disposeBack = registerPullsBackHandler(stepBack);
  onDestroy(() => {
    disposeBack();
    disposeProbe();
    detachPullsPage(paneId);
  });

  function stepBack(): boolean {
    if (tab === 'mrs' && selection) {
      clearSelection(paneId);
      return true;
    }
    if (tab === 'pipelines' && pipeline) {
      clearPipelineSelection(paneId);
      return true;
    }
    return false;
  }

  function setTab(next: Tab): void {
    tab = next;
    appStorageSet(TAB_KEY, next);
  }

  function readListWidth(): number {
    const raw = Number(appStorageGet(LIST_WIDTH_KEY));
    return Number.isFinite(raw) && raw > 0 ? clampListWidth(raw) : LIST_DEFAULT_PX;
  }

  function clampListWidth(px: number): number {
    return Math.min(LIST_MAX_PX, Math.max(LIST_MIN_PX, Math.round(px)));
  }

  function startListResize(event: PointerEvent): void {
    event.preventDefault();
    const handle = event.currentTarget as HTMLElement;
    const startX = event.clientX;
    const startWidth = listWidth;
    handle.setPointerCapture(event.pointerId);
    const onMove = (move: PointerEvent) => {
      listWidth = clampListWidth(startWidth + (move.clientX - startX));
    };
    const onEnd = () => {
      handle.removeEventListener('pointermove', onMove);
      handle.removeEventListener('pointerup', onEnd);
      handle.removeEventListener('pointercancel', onEnd);
      appStorageSet(LIST_WIDTH_KEY, String(listWidth));
    };
    handle.addEventListener('pointermove', onMove);
    handle.addEventListener('pointerup', onEnd);
    handle.addEventListener('pointercancel', onEnd);
  }

  function resetListWidth(): void {
    listWidth = LIST_DEFAULT_PX;
    appStorageSet(LIST_WIDTH_KEY, String(listWidth));
  }

  function openRow(row: PullRowView): void {
    selectPR(paneId, row.key);
  }

  function openPipelineRow(row: PipelineRowView): void {
    selectPipeline(paneId, row.key);
  }

  function refresh(): void {
    if (tab === 'mrs') void refreshPullsPage(paneId);
    else refreshPipelinesPage(paneId);
  }

  // The agent menu: from the header's button on the picked MR, or from a
  // row's menu, which picks that row first so the menu and the store agree
  // on which MR it is about.
  let agentOpen = $state(false);
  let agentAnchor: HTMLElement | undefined = $state(undefined);
  let agentHeaderEl: HTMLElement | undefined = $state(undefined);
  let agentBusy = $state(false);
  const agentThreads = $derived(threadsOnPR(paneId));
  const agentCheckedOutBy = $derived(newThreadCheckedOutBy(paneId));
  const agentProjectThreads = $derived(selection ? projectThreads(selection.projectId, backend) : []);
  const agentBranch = $derived(selection ? (selection.row.crossRepository ? `pr/${selection.pr.number}` : selection.row.headRefName) : '');
  const draftsCount = $derived(currentPinnedReviewState(paneId)?.drafts.length ?? 0);

  function openAgentMenu(anchor: HTMLElement): void {
    agentAnchor = anchor;
    agentOpen = true;
    void loadThreadsOnPR(paneId);
  }

  function closeAgentMenu(reason?: PopoverCloseReason): void {
    agentOpen = false;
    restorePickerFocus(reason, { triggerEl: agentAnchor });
  }

  function openRowMenu(row: PullRowView, anchor: HTMLElement): void {
    selectPR(paneId, row.key);
    openAgentMenu(anchor);
  }

  async function runAgent(action: () => Promise<boolean>): Promise<void> {
    agentBusy = true;
    try {
      await action();
    } finally {
      agentBusy = false;
    }
  }

  async function openOnForge(): Promise<void> {
    const url = selection?.row.url;
    if (!url) return;
    try {
      await OpenExternalURL(url);
    } catch (err) {
      addErrorToast(`Could not open the pull request: ${errString(err)}`, err);
    }
  }

  const emptyText = $derived.by(() => {
    switch (filters.filter) {
      case 'needs_you':
        return 'Nothing needs you right now.';
      case 'mine':
        return 'No open merge requests of yours.';
      case 'review_requested':
        return 'No reviews requested from you.';
      default:
        return 'No open merge requests.';
    }
  });
</script>

<div class="flex h-full min-h-0 flex-col" data-testid="pulls-pane" data-tab={tab}>
  <ToolPaneHeader
    {paneId}
    kind="pulls"
    {onPaneDragStart}
    onBack={compact && detailShown ? () => { stepBack(); } : undefined}
    backLabel="Back to the list"
    backTestId="pulls-back"
  >
    <div class="flex min-w-0 flex-1 items-center gap-2">
      {#if !(compact && detailShown)}
        <Segmented
          ariaLabel="Pull requests tab"
          size="sm"
          value={tab}
          onChange={setTab}
          options={[
            { value: 'mrs', label: 'Merge requests' },
            { value: 'pipelines', label: 'Pipelines' },
          ]}
        />
      {/if}
      <div class="ml-auto flex shrink-0 items-center gap-1">
        {#if tab === 'mrs' && selection}
          <span bind:this={agentHeaderEl} class="inline-flex">
            <Button
              variant="tinted"
              size="xs"
              testId="pulls-open-with-agent"
              ariaLabel="Open with agent"
              onclick={() => { if (agentHeaderEl) openAgentMenu(agentHeaderEl); }}
            >
              {#snippet leading()}<Icon icon={Sparkles} size={12} strokeWidth={1.75} />{/snippet}
              <span class="compact:hidden">Open with agent</span>
              {#snippet trailing()}<Icon icon={ChevronDown} size={11} strokeWidth={2} />{/snippet}
            </Button>
          </span>
        {/if}
        <PaneHeaderIconButton
          label={tab === 'mrs' ? 'Refresh merge requests' : 'Refresh pipelines'}
          testId="pulls-refresh"
          onclick={refresh}
        >
          <Icon icon={RefreshCw} size={13} strokeWidth={1.75} class={(tab === 'mrs' ? pullsRefreshing(paneId) : pipelinesReading(paneId)) ? 'animate-spin' : ''} />
        </PaneHeaderIconButton>
      </div>
    </div>
  </ToolPaneHeader>

  <div class="flex min-h-0 flex-1">
    <!-- The list column: the page on compact, a resizable column beside the detail on desktop. -->
    <div
      class="relative flex min-h-0 shrink-0 flex-col border-r border-border-subtle compact:w-full compact:border-r-0 {compact && detailShown ? 'hidden' : ''}"
      style:width={compact ? undefined : `${listWidth}px`}
      data-testid="pulls-list-column"
    >
      {#if tab === 'mrs'}
        <PullsFilterChips
          filter={filters.filter}
          needsYouCount={pullsNeedsYouCount(paneId)}
          projects={pullsProjects(paneId)}
          selectedProjects={filters.projects}
          onFilter={(next) => setMRFilters(backend, { filter: next })}
          onProjects={(next) => setMRFilters(backend, { projects: next })}
        />
        {#if pullsSnapshotError(paneId)}
          <div class="border-y border-error/30 bg-error/10 px-3 py-2 text-xs text-error" data-testid="pulls-snapshot-error">{pullsSnapshotError(paneId)}</div>
        {/if}
        <div
          class="min-h-0 flex-1 overflow-y-auto border-t border-border-subtle"
          data-testid="pulls-list"
          use:visibleRows={{ attr: 'pr-key', onVisible: (keys) => setFocusedRows(paneId, keys) }}
        >
          {#each projectErrors as failed (failed.projectId)}
            <PullsGroupHeader label={failed.name} testid="pulls-project-group" />
            <PullsProjectError message={failed.message} failure={failed.failure} forge={failed.forge} onRetry={refresh} retrying={pullsRefreshing(paneId)} />
          {/each}
          {#each groups as group (group.group)}
            <PullsGroupHeader label={pullGroupLabel(group.group)} count={group.rows.length} testid="pulls-group-{group.group}" />
            {#each group.rows as row (row.key)}
              <PullRow {row} selected={selection?.key === row.key} onOpen={openRow} onMenu={openRowMenu} />
            {/each}
          {/each}
          {#if pullsError(paneId)}
            <p class="px-4 py-6 text-xs text-error" data-testid="pulls-error">{pullsError(paneId)}</p>
          {:else if pullsLoading(paneId) && rowCount === 0 && projectErrors.length === 0}
            <p class="px-4 py-6 text-xs text-fg-muted" data-testid="pulls-loading">Reading merge requests…</p>
          {:else if rowCount === 0 && projectErrors.length === 0}
            <div class="flex flex-col items-center gap-1 px-6 py-10 text-center" data-testid="pulls-empty">
              <p class="text-sm text-fg-muted">{pullsProjects(paneId).length === 0 ? 'No project on this computer has a GitHub or GitLab origin.' : emptyText}</p>
            </div>
          {/if}
        </div>
      {:else}
        <PipelinesFilterChips filters={pipeFilters} projects={pipelinesProjects(paneId)} onChange={(next) => setPipelinesFilters(paneId, next)} />
        <div
          class="min-h-0 flex-1 overflow-y-auto border-t border-border-subtle"
          data-testid="pipelines-list"
          use:visibleRows={{ attr: 'pipeline-key', onVisible: (keys) => setVisiblePipelines(paneId, keys) }}
        >
          {#each pipelinesProjectErrors(paneId) as failed (failed.projectId)}
            <PullsGroupHeader label={failed.name} testid="pipelines-project-group" />
            <PullsProjectError message={failed.message} failure={failed.failure} forge={failed.forge} onRetry={refresh} retrying={pipelinesReading(paneId)} />
          {/each}
          {#each pipeRows as row (row.key)}
            <PipelineRow {row} selected={pipeline?.key === row.key} onOpen={openPipelineRow} />
          {/each}
          {#if pipelinesError(paneId)}
            <p class="px-4 py-6 text-xs text-error" data-testid="pipelines-error">{pipelinesError(paneId)}</p>
          {:else if pipelinesLoading(paneId) && pipeRows.length === 0}
            <p class="px-4 py-6 text-xs text-fg-muted" data-testid="pipelines-loading">Reading pipelines…</p>
          {:else if pipeRows.length === 0 && pipelinesProjectErrors(paneId).length === 0}
            <div class="flex flex-col items-center gap-1 px-6 py-10 text-center" data-testid="pipelines-empty">
              <p class="text-sm text-fg-muted">No pipelines match.</p>
            </div>
          {/if}
        </div>
      {/if}
      {#if !compact}
        <!-- svelte-ignore a11y_no_noninteractive_element_interactions -->
        <div
          role="separator"
          aria-orientation="vertical"
          aria-label="Resize the list"
          class="absolute inset-y-0 -right-0.5 z-10 w-1 cursor-col-resize hover:bg-accent/40 active:bg-accent/60"
          data-testid="pulls-list-resize"
          onpointerdown={startListResize}
          ondblclick={resetListWidth}
        ></div>
      {/if}
    </div>

    <!-- The detail: the review pinned to the picked MR, or the picked pipeline. -->
    <div class="flex min-h-0 min-w-0 flex-1 flex-col {compact && !detailShown ? 'hidden' : ''}" data-testid="pulls-detail">
      {#if tab === 'mrs'}
        {#if selection}
          {#key selection.key}
            <ReviewPane
              pr={{ ownerPaneId: paneId, backend: selection.backend, projectId: selection.projectId, pr: selection.pr }}
            />
          {/key}
        {:else}
          <div class="flex flex-1 items-center justify-center px-6" data-testid="pulls-detail-empty">
            <p class="text-sm text-fg-muted">Pick a merge request to review it here.</p>
          </div>
        {/if}
      {:else if pipeline}
        {#key pipeline.key}
          <PipelineView row={pipeline.row} ci={pipeline.ci} />
        {/key}
      {:else}
        <div class="flex flex-1 items-center justify-center px-6" data-testid="pipelines-detail-empty">
          <p class="text-sm text-fg-muted">Pick a pipeline to see its stages and logs.</p>
        </div>
      {/if}
    </div>
  </div>
</div>

{#if selection}
  <OpenWithAgentMenu
    anchor={agentAnchor}
    open={agentOpen}
    onClose={closeAgentMenu}
    branch={agentBranch}
    crossRepository={selection.row.crossRepository}
    threadsOnPR={agentThreads}
    projectThreads={agentProjectThreads}
    {draftsCount}
    checkedOutBy={agentCheckedOutBy}
    busy={agentBusy || threadsOnPRLoading(paneId)}
    onNewThread={() => { void runAgent(() => openPRThread(paneId)); }}
    onOpenThread={(threadId) => { void runAgent(() => openThread(paneId, threadId)); }}
    onSendDrafts={(threadId) => { void runAgent(() => sendDrafts(paneId, threadId)); }}
  >
    {#snippet extra()}
      <MenuItem label="Open on {forgeLabels(selection?.pr.forge).name}" onSelect={() => { void openOnForge(); }} testId="pull-menu-open-forge" />
      <MenuItem
        label="Copy URL"
        onSelect={() => { if (selection) void copyToClipboard(selection.row.url); }}
        testId="pull-menu-copy-url"
      />
    {/snippet}
  </OpenWithAgentMenu>
{/if}
