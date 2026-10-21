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
  import PullsErrorNotice from './PullsErrorNotice.svelte';
  import PullsListResizer from './PullsListResizer.svelte';
  import PullsProjectError from './PullsProjectError.svelte';
  import { ListColumnWidth } from './listWidth.svelte';
  import { provideHostedBack } from '../panes/hostedBack';
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
    listedPR,
    pullsError,
    pullsLoading,
    pullsNeedsYouCount,
    pullsProjectErrors,
    pullsProjects,
    pullsRefreshFailure,
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
    threadsOnPRError,
    threadsOnPRLoading,
  } from '../../stores/pullsPageAgent.svelte';
  import { mrFilters, pipelineFilters, setMRFilters } from '../../stores/pullsPageFilters.svelte';
  import type { CapturedError } from '../../stores/errorReports.svelte';
  import { currentPinnedReviewState, reviewSubjectForPR, type ReviewPaneState } from '../../stores/reviewPane.svelte';
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
  // page and the detail full width with the header's Back, the one Back:
  // it closes a view open in the detail (a CI log, the conflicts) before
  // the detail. The page lists the computer selected when it opened and
  // keeps it for the pane's life.

  interface Props {
    paneId: string;
    onPaneDragStart?: (event: DragEvent) => void;
  }

  let { paneId, onPaneDragStart }: Props = $props();

  type Tab = 'mrs' | 'pipelines';
  const TAB_KEY = 'pulls:tab';
  const LIST_WIDTH_KEY = 'pullsListWidth';

  const backend = untrack(() => selectedBackend());
  let tab: Tab = $state(appStorageGet(TAB_KEY) === 'pipelines' ? 'pipelines' : 'mrs');
  let compact = $derived(isCompactLayout());
  const listColumn = new ListColumnWidth(LIST_WIDTH_KEY);
  provideHostedBack(() => compact);

  const selection = $derived(selectedPR(paneId));
  // The pinned review's subject, one nullable value the template reads
  // through its guard: the pane id and the selection never split into
  // two render effects.
  const pinnedReview = $derived.by(() => {
    if (!selection) return null;
    return { key: selection.key, subject: { ownerPaneId: paneId, backend: selection.backend, projectId: selection.projectId, pr: selection.pr } };
  });
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

  // The selected MR's review state, when it is the one the page pins.
  function selectedReview(): ReviewPaneState | null {
    if (!pinnedReview) return null;
    const state = currentPinnedReviewState(paneId);
    try {
      return state?.identity === reviewSubjectForPR(pinnedReview.subject).identity ? state : null;
    } catch {
      return null;
    }
  }

  // One step back: a view open in the detail closes first, then the detail.
  function stepBack(): boolean {
    if (tab === 'mrs' && selection) {
      const review = selectedReview();
      if (review?.ciLogView) review.closeCILogView();
      else if (review?.conflictView) review.closeConflictView();
      else clearSelection(paneId);
      return true;
    }
    if (tab === 'pipelines' && pipeline) {
      if (pipeline.ci.logView) pipeline.ci.closeLogView();
      else clearPipelineSelection(paneId);
      return true;
    }
    return false;
  }

  function setTab(next: Tab): void {
    tab = next;
    appStorageSet(TAB_KEY, next);
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

  // A failed refresh of the merge requests reads under the header until
  // dismissed or the next refresh. The Pipelines tab's failures land in
  // its list and its project rows.
  const refreshFailure = $derived(tab === 'mrs' ? pullsRefreshFailure(paneId) : null);
  let dismissedRefresh: CapturedError | null = $state.raw(null);

  // The agent menu: from the header's button on the picked MR, or from a
  // row's menu on that row's MR, which stays unpicked so the list and the
  // menu's anchor stay on screen. `agentRowKey` is the row's key, or
  // undefined for the picked MR.
  let agentOpen = $state(false);
  let agentAnchor: HTMLElement | undefined = $state(undefined);
  let agentRowKey: string | undefined = $state(undefined);
  let agentBusy = $state(false);
  const agentTarget = $derived(agentRowKey === undefined ? selection : listedPR(paneId, agentRowKey));
  const agentThreads = $derived(threadsOnPR(paneId, agentRowKey));
  const agentCheckedOutBy = $derived(newThreadCheckedOutBy(paneId, agentRowKey));
  const agentProjectThreads = $derived(agentTarget ? projectThreads(agentTarget.projectId, backend) : []);
  const agentBranch = $derived(agentTarget ? (agentTarget.row.crossRepository ? `pr/${agentTarget.pr.number}` : agentTarget.row.headRefName) : '');
  // The drafts are the pinned review's, which is the picked MR's.
  const draftsCount = $derived(agentTarget && agentTarget.key === selection?.key ? (currentPinnedReviewState(paneId)?.drafts.length ?? 0) : 0);

  function openAgentMenu(anchor: HTMLElement, rowKey?: string): void {
    agentAnchor = anchor;
    agentRowKey = rowKey;
    agentOpen = true;
    void loadThreadsOnPR(paneId, rowKey);
  }

  function closeAgentMenu(reason?: PopoverCloseReason): void {
    agentOpen = false;
    restorePickerFocus(reason, { triggerEl: agentAnchor });
  }

  function openRowMenu(row: PullRowView, anchor: HTMLElement): void {
    openAgentMenu(anchor, row.key);
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
    const url = agentTarget?.row.url;
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
    backLabel="Back"
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
          <Button
            variant="tinted"
            size="xs"
            testId="pulls-open-with-agent"
            ariaLabel="Open with agent"
            onclick={(event) => openAgentMenu(event.currentTarget as HTMLElement)}
          >
            {#snippet leading()}<Icon icon={Sparkles} size={12} strokeWidth={1.75} />{/snippet}
            <span class="compact:hidden">Open with agent</span>
            {#snippet trailing()}<Icon icon={ChevronDown} size={11} strokeWidth={2} />{/snippet}
          </Button>
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
  {#if refreshFailure && refreshFailure !== dismissedRefresh}
    <PullsErrorNotice
      message="Could not refresh the merge requests: {refreshFailure.report.reason}"
      captured={refreshFailure}
      onDismiss={() => { dismissedRefresh = refreshFailure; }}
      testId="pulls-refresh-error"
    />
  {/if}

  <div class="flex min-h-0 flex-1" bind:clientWidth={listColumn.paneWidth}>
    <!-- The list column: the page on compact, a resizable column beside the detail on desktop. -->
    <div
      class="relative flex min-h-0 shrink-0 flex-col border-r border-border-subtle compact:w-full compact:border-r-0 {compact && detailShown ? 'hidden' : ''}"
      style:width={compact ? undefined : `${listColumn.width}px`}
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
            <PullsProjectError message={failed.message} failure={failed.failure} forge={failed.forge} cause={failed.cause} onRetry={refresh} retrying={pullsRefreshing(paneId)} />
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
            <PullsProjectError message={failed.message} failure={failed.failure} forge={failed.forge} cause={failed.cause} onRetry={refresh} retrying={pipelinesReading(paneId)} />
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
        <PullsListResizer column={listColumn} />
      {/if}
    </div>

    <!-- The detail: the review pinned to the picked MR, or the picked pipeline. -->
    <div class="flex min-h-0 min-w-0 flex-1 flex-col {compact && !detailShown ? 'hidden' : ''}" data-testid="pulls-detail">
      {#if tab === 'mrs'}
        {#if pinnedReview}
          {#key pinnedReview.key}
            <ReviewPane pr={pinnedReview.subject} />
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

{#if agentTarget}
  <OpenWithAgentMenu
    anchor={agentAnchor}
    open={agentOpen}
    onClose={closeAgentMenu}
    branch={agentBranch}
    crossRepository={agentTarget.row.crossRepository}
    threadsOnPR={agentThreads}
    threadsLoading={threadsOnPRLoading(paneId, agentRowKey)}
    threadsError={threadsOnPRError(paneId, agentRowKey)}
    onRetryThreads={() => { void loadThreadsOnPR(paneId, agentRowKey); }}
    projectThreads={agentProjectThreads}
    {draftsCount}
    checkedOutBy={agentCheckedOutBy}
    busy={agentBusy}
    onNewThread={() => { void runAgent(() => openPRThread(paneId, agentRowKey)); }}
    onOpenThread={(threadId) => { void runAgent(() => openThread(paneId, threadId)); }}
    onSendDrafts={(threadId) => { void runAgent(() => sendDrafts(paneId, threadId)); }}
  >
    {#snippet extra()}
      <MenuItem label="Open on {forgeLabels(agentTarget?.pr.forge).name}" onSelect={() => { void openOnForge(); }} testId="pull-menu-open-forge" />
      <MenuItem
        label="Copy URL"
        onSelect={() => { if (agentTarget) void copyToClipboard(agentTarget.row.url); }}
        testId="pull-menu-copy-url"
      />
    {/snippet}
  </OpenWithAgentMenu>
{/if}
