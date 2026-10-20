<script lang="ts">
  import ChevronDown from '@lucide/svelte/icons/chevron-down';
  import Button from '../primitives/Button.svelte';
  import Icon from '../primitives/Icon.svelte';
  import Menu from '../primitives/Menu.svelte';
  import MenuDivider from '../primitives/MenuDivider.svelte';
  import MenuItem from '../primitives/MenuItem.svelte';
  import Popover from '../primitives/Popover.svelte';
  import ReviewCollapsibleSection from './ReviewCollapsibleSection.svelte';
  import ReviewResizableBody from './ReviewResizableBody.svelte';
  import { restorePickerFocus } from '../panes/paneComposerFocus';
  import { OpenExternalURL } from '../../stores/bindings';
  import { relativeTime } from '../../utils/format';
  import { authorDisplayName } from '../../utils/reviewIdentity';
  import { READINESS_PILL_CLASS, mergeMethodLabel, readinessPill } from '../../utils/reviewReadiness';
  import type { PopoverCloseReason } from '../../utils/popoverOwnership';
  import type { ReviewPaneState } from '../../stores/reviewPane.svelte';
  import type { CIJob, MergeBlocker, PRDetail } from '../../types/models';

  // The Merge section: the overview's first collapsible and the one home
  // of approve, merge and auto-merge on every surface. The header reads
  // the readiness pill (the same one the title bar's refs line carries),
  // Approve or Unapprove, and the Merge menu; the body lists what stands
  // in the way, each blocker beside its fix, then what is already met,
  // so the section is never empty. Nothing here is optimistic: every
  // action asks the forge, the pump polls, and the rows follow the
  // forge's answer (docs/specs/pull-requests.md).

  interface Props {
    review: ReviewPaneState;
    detail: PRDetail;
    /** A checkout exists, so conflicts can open in the conflict view. */
    hasWorkspace?: boolean;
  }

  let { review, detail, hasWorkspace = false }: Props = $props();

  const pill = $derived(readinessPill(detail));
  const readiness = $derived(detail.readiness);
  const merge = $derived(detail.merge);
  const mergeState = $derived(readiness?.state ?? '');
  const settledPR = $derived(mergeState === 'merged' || mergeState === 'closed');
  const armed = $derived(merge?.autoMerge ?? null);
  const pending = $derived(review.mergeAction);
  const withheld = $derived(review.mergeWithheldReason);
  const busy = $derived(pending !== null || withheld !== null);
  const blockers = $derived<readonly MergeBlocker[]>(readiness?.blockers ?? []);
  const settled = $derived<readonly MergeBlocker[]>(readiness?.settled ?? []);
  const methods = $derived<readonly string[]>(merge?.methods?.length ? merge.methods : ['merge']);

  // The menu's picks live with the section: the project's default until
  // the reader picks, falling back when a poll drops the pick's method.
  let pickedMethod: string | null = $state(null);
  let pickedDelete: boolean | null = $state(null);
  const method = $derived(pickedMethod !== null && methods.includes(pickedMethod) ? pickedMethod : methods[0]);
  const deleteBranch = $derived(merge?.deleteBranchOffered ? (pickedDelete ?? merge.deleteBranchDefault) : false);

  const showApprove = $derived(!settledPR && !!merge && (merge.viewerCanApprove || merge.viewerApproved));
  const showMerge = $derived(!settledPR && !!merge && merge.viewerCanMerge);
  const canMergeNow = $derived(mergeState === 'ready' && armed === null);
  const canAutoMerge = $derived(!!merge?.autoMergeAvailable && armed === null && mergeState !== 'ready' && mergeState !== 'draft');
  const mergeLabel = $derived.by(() => {
    if (armed) return 'Auto-merge set';
    if (canMergeNow || !canAutoMerge) return 'Merge';
    return 'Auto-merge when ready';
  });
  const mergePending = $derived(pending === 'merge' || pending === 'auto_merge' || pending === 'cancel_auto_merge');
  const approvePending = $derived(pending === 'approve' || pending === 'unapprove');
  // "Merge now" names what it waits for while blocked.
  const blockedSummary = $derived(blockers.map((blocker) => blocker.text).join(' · '));

  let menuOpen = $state(false);
  let triggerEl: HTMLButtonElement | undefined = $state(undefined);

  function closeMenu(reason?: PopoverCloseReason): void {
    menuOpen = false;
    restorePickerFocus(reason, { triggerEl });
  }

  function pickAndClose(action: () => void): void {
    menuOpen = false;
    action();
  }

  function time(at: string | undefined): string {
    const ms = Date.parse(at ?? '');
    return Number.isNaN(ms) ? '' : relativeTime(ms);
  }

  async function openURL(url: string | undefined): Promise<void> {
    if (!url) return;
    await OpenExternalURL(url);
  }

  // The pipeline's job that explains a checks blocker: a failed one, else
  // one still running; the log opens in the pane. Without one the
  // forge's page is the answer.
  function pipelineJob(): { stageName: string; job: CIJob } | null {
    let running: { stageName: string; job: CIJob } | null = null;
    for (const stage of review.ciPipeline?.stages ?? []) {
      for (const job of stage.jobs) {
        if (job.status === 'failed' && job.logsAvailable) return { stageName: stage.name, job };
        if (!running && job.status === 'running' && job.logsAvailable) running = { stageName: stage.name, job };
      }
    }
    return running;
  }

  interface RowAction {
    label: string;
    testid: string;
    run: () => void;
    /** Reads as a merge action (disabled while one is in flight). */
    merge?: boolean;
  }

  // Each blocker kind's fix, when this viewer has one.
  function blockerAction(blocker: MergeBlocker): RowAction | null {
    switch (blocker.kind) {
      case 'approvals':
        if (merge?.viewerCanApprove && !merge.viewerApproved) {
          return { label: 'Approve', testid: 'review-merge-row-approve', run: () => { void review.approvePR(); }, merge: true };
        }
        return null;
      case 'checks': {
        const job = pipelineJob();
        if (job) return { label: 'Open pipeline', testid: 'review-merge-row-pipeline', run: () => review.openCIJobLog(job.stageName, job.job) };
        const url = blocker.url || review.ciPipeline?.url;
        return url ? { label: 'Open pipeline', testid: 'review-merge-row-pipeline', run: () => { void openURL(url); } } : null;
      }
      case 'threads': {
        const first = review.unresolvedThreads[0];
        return first ? { label: 'Jump', testid: 'review-merge-row-threads', run: () => review.jumpToConversationThread(first.id) } : null;
      }
      case 'conflicts':
        if (hasWorkspace) return { label: 'View conflicts', testid: 'review-merge-row-conflicts', run: () => { void review.openConflictView(); } };
        return blocker.url ? { label: 'Open', testid: 'review-merge-row-conflicts', run: () => { void openURL(blocker.url); } } : null;
      case 'rebase':
        if (merge?.viewerCanMerge) return { label: 'Update branch', testid: 'review-merge-row-rebase', run: () => { void review.updatePRBranch(); }, merge: true };
        return null;
      case 'draft':
        if (merge?.viewerCanMerge || detail.viewerIsAuthor) return { label: 'Mark ready', testid: 'review-merge-row-ready', run: () => { void review.markPRReady(); }, merge: true };
        return null;
      default:
        return blocker.url ? { label: 'Open', testid: 'review-merge-row-open', run: () => { void openURL(blocker.url); } } : null;
    }
  }

  function blockerDot(blocker: MergeBlocker): string {
    switch (blocker.kind) {
      case 'conflicts':
      case 'changes_requested':
        return 'bg-error';
      case 'checks':
        return review.ciPipeline?.status === 'running' ? 'bg-accent animate-pulse' : 'bg-warning';
      default:
        return 'bg-warning';
    }
  }

  // The merged and closed rows' sentences, so the if-blocks never eat a space.
  const mergedLine = $derived.by(() => {
    const parts = ['Merged'];
    if (detail.mergedBy) parts.push(`by ${detail.mergedBy}`);
    const when = time(detail.mergedAt);
    return when ? `${parts.join(' ')} · ${when}` : parts.join(' ');
  });
  const closedLine = $derived.by(() => {
    const when = time(detail.closedAt);
    return when ? `Closed without merging · ${when}` : 'Closed without merging';
  });
  const rowClass = 'flex min-h-9 items-center gap-2.5 px-3 py-1.5 text-xs';
  const actionClass = 'shrink-0';
  // The trigger is a native button (the menu anchors to it) in the Button
  // primitive's xs clothes.
  const triggerClass = 'inline-flex h-6 shrink-0 items-center justify-center gap-1 whitespace-nowrap rounded-[var(--radius-control)] px-2 text-[0.6875rem] font-medium select-none transition-[color,background-color,border-color,opacity] duration-150 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/40 disabled:cursor-not-allowed disabled:opacity-50 compact:h-9 compact:px-2.5 compact:select-none';
  const TRIGGER_PRIMARY = 'bg-accent text-surface-0 hover:opacity-90';
  const TRIGGER_SECONDARY = 'border border-border-subtle bg-transparent text-fg-muted hover:border-border hover:bg-surface-2/40 hover:text-fg';
</script>

<ReviewCollapsibleSection
  label="Merge"
  open={review.mergeOpen}
  onToggle={() => review.setMergeOpen(!review.mergeOpen)}
  testid="review-pr-merge"
>
  {#snippet badge()}
    {#if pill}
      <span class="shrink-0 rounded-full px-2 py-px text-[0.625rem] font-normal {READINESS_PILL_CLASS[pill.tone]}" data-testid="review-merge-pill" data-state={pill.state}>{pill.label}</span>
    {/if}
  {/snippet}
  {#snippet trailing()}
    {#if showApprove}
      <Button
        variant="secondary"
        size="xs"
        testId="review-merge-approve"
        loading={approvePending}
        disabled={busy}
        title={withheld ?? (merge?.viewerApproved ? 'Withdraw your approval' : 'Approve this pull request as it is now')}
        ariaLabel={merge?.viewerApproved ? 'Withdraw approval' : 'Approve pull request'}
        onclick={() => { void (merge?.viewerApproved ? review.unapprovePR() : review.approvePR()); }}
      >
        {merge?.viewerApproved ? 'Unapprove' : 'Approve'}
      </Button>
    {/if}
    {#if showMerge}
      <!-- Never disabled while the PR is open: blocked, its first action
           is auto-merge; armed, it cancels. The menu names what each
           row waits for. -->
      <button
        bind:this={triggerEl}
        type="button"
        class="{triggerClass} {canMergeNow && !armed ? TRIGGER_PRIMARY : TRIGGER_SECONDARY} {menuOpen ? 'ring-1 ring-inset ring-border' : ''}"
        aria-label="Merge options"
        title={withheld ?? undefined}
        aria-haspopup="menu"
        aria-expanded={menuOpen}
        aria-busy={mergePending ? 'true' : undefined}
        data-testid="review-merge-trigger"
        disabled={busy}
        onclick={() => { menuOpen = !menuOpen; }}
      >
        {#if mergePending}
          <span class="inline-block size-3 animate-spin rounded-full border border-current border-r-transparent" aria-hidden="true"></span>
        {/if}
        <!-- The phone's row has no room for the long label. -->
        <span class="compact:hidden">{mergeLabel}</span>
        <span class="hidden compact:inline">{mergeLabel === 'Auto-merge when ready' ? 'Auto-merge' : mergeLabel}</span>
        <Icon icon={ChevronDown} size={12} class="shrink-0 opacity-80" />
      </button>
    {/if}
  {/snippet}
  <ReviewResizableBody
    section="merge"
    fallbackClass="max-h-[min(40vh,22rem)]"
    scrollTop={review.overviewSectionScrollTop('merge')}
    onScrollTop={(px) => review.setOverviewSectionScrollTop('merge', px)}
  >
    <div class="flex flex-col divide-y divide-border-subtle" data-testid="review-merge-rows">
      {#if mergeState === 'merged'}
        <div class={rowClass} data-testid="review-merge-settled" data-kind="merged">
          <span class="size-2 shrink-0 rounded-full bg-accent"></span>
          <span class="min-w-0 flex-1 truncate text-fg">{mergedLine}</span>
        </div>
      {:else if mergeState === 'closed'}
        <div class={rowClass} data-testid="review-merge-settled" data-kind="closed">
          <span class="size-2 shrink-0 rounded-full bg-error"></span>
          <span class="min-w-0 flex-1 truncate text-fg">{closedLine}</span>
        </div>
      {/if}
      {#if armed && !settledPR}
        <div class={rowClass} data-testid="review-merge-armed">
          <span class="size-2 shrink-0 rounded-full bg-accent"></span>
          <span class="min-w-0 flex-1">
            <span class="block truncate text-fg">Auto-merge set by {authorDisplayName({ authorLogin: armed.byLogin, authorName: armed.byName })} · {mergeMethodLabel(armed.method)}{#if time(armed.at)} · {time(armed.at)}{/if}</span>
            {#if armed.waitsFor}
              <span class="block truncate text-[0.6875rem] text-fg-muted compact:whitespace-normal compact:break-words">Merges once {armed.waitsFor}</span>
            {/if}
          </span>
          {#if armed.canCancel}
            <Button variant="ghost" size="xs" class={actionClass} testId="review-merge-row-cancel-auto" disabled={busy} onclick={() => { void review.cancelPRAutoMerge(); }}>Cancel</Button>
          {/if}
        </div>
      {/if}
      {#each blockers as blocker, index (`${blocker.kind}:${index}`)}
        {@const action = blockerAction(blocker)}
        <div class={rowClass} data-testid="review-merge-blocker" data-kind={blocker.kind}>
          <span class="size-2 shrink-0 rounded-full {blockerDot(blocker)}"></span>
          <span class="min-w-0 flex-1">
            <span class="block truncate text-fg compact:whitespace-normal compact:break-words" title={blocker.text}>{blocker.text}</span>
            {#if blocker.detail}
              <span class="block truncate text-[0.6875rem] text-fg-muted compact:whitespace-normal compact:break-words" title={blocker.detail}>{blocker.detail}</span>
            {/if}
          </span>
          {#if blocker.kind === 'approvals' && merge?.viewerApproved}
            <span class="shrink-0 text-[0.6875rem] text-fg-subtle" data-testid="review-merge-you-approved">you approved</span>
          {:else if blocker.kind === 'approvals' && merge && !merge.viewerCanApprove && merge.cannotApproveReason}
            <!-- The row says why there is no Approve beside it. -->
            <span class="min-w-0 text-[0.6875rem] text-fg-subtle" data-testid="review-merge-cannot-approve">{merge.cannotApproveReason}</span>
          {/if}
          {#if action}
            <Button variant="ghost" size="xs" class={actionClass} testId={action.testid} disabled={action.merge ? busy : false} onclick={action.run}>{action.label}</Button>
          {/if}
        </div>
      {/each}
      {#each settled as row, index (`s:${row.kind}:${index}`)}
        <div class={rowClass} data-testid="review-merge-settled" data-kind={row.kind}>
          <span class="size-2 shrink-0 rounded-full bg-success"></span>
          <span class="min-w-0 flex-1">
            <span class="block truncate text-fg-muted compact:whitespace-normal compact:break-words" title={row.text}>{row.text}</span>
            {#if row.detail}
              <span class="block truncate text-[0.6875rem] text-fg-subtle compact:whitespace-normal compact:break-words" title={row.detail}>{row.detail}</span>
            {/if}
          </span>
          {#if row.kind === 'approvals' && merge?.viewerApproved && !settledPR}
            <span class="shrink-0 text-[0.6875rem] text-fg-subtle" data-testid="review-merge-you-approved">you approved</span>
          {/if}
        </div>
      {/each}
      {#if !settledPR && merge && !merge.viewerCanMerge && merge.cannotMergeReason}
        <div class={rowClass} data-testid="review-merge-cannot">
          <span class="size-2 shrink-0 rounded-full bg-fg-subtle"></span>
          <span class="min-w-0 flex-1 truncate text-fg-muted compact:whitespace-normal compact:break-words" title={merge.cannotMergeReason}>{merge.cannotMergeReason}</span>
        </div>
      {/if}
      {#if !settledPR && merge && !merge.viewerCanApprove && !merge.viewerApproved && merge.cannotApproveReason && !blockers.some((blocker) => blocker.kind === 'approvals')}
        <div class={rowClass} data-testid="review-merge-cannot-approve">
          <span class="size-2 shrink-0 rounded-full bg-fg-subtle"></span>
          <span class="min-w-0 flex-1 truncate text-fg-muted">{merge.cannotApproveReason}</span>
        </div>
      {/if}
      {#if mergeState === 'checking' && blockers.length === 0 && settled.length === 0}
        <div class={rowClass} data-testid="review-merge-checking">
          <span class="size-2 shrink-0 animate-pulse rounded-full bg-accent"></span>
          <span class="min-w-0 flex-1 truncate text-fg-muted">The forge is checking whether this can merge.</span>
        </div>
      {/if}
    </div>
  </ReviewResizableBody>
</ReviewCollapsibleSection>

{#if showMerge}
  <Popover anchor={triggerEl} open={menuOpen} onClose={closeMenu} placement="bottom-end" role="none">
    <Menu ariaLabel="Merge options" onClose={closeMenu} minWidthClass="min-w-[240px]">
      <div data-testid="review-merge-menu">
        {#each methods as candidate, index (candidate)}
          <MenuItem
            label={mergeMethodLabel(candidate)}
            description={index === 0 ? 'project default' : undefined}
            checked={candidate === method}
            testId="review-merge-method-{candidate}"
            onSelect={() => { pickedMethod = candidate; }}
          />
        {/each}
        {#if merge?.deleteBranchOffered}
          <MenuDivider />
          <MenuItem
            label="Delete source branch"
            checkbox
            checked={deleteBranch}
            testId="review-merge-delete-branch"
            onSelect={() => { pickedDelete = !deleteBranch; }}
          />
        {/if}
        <MenuDivider />
        <MenuItem
          label="Merge now"
          description={canMergeNow ? undefined : armed ? 'auto-merge is set' : blockedSummary || undefined}
          disabled={!canMergeNow}
          testId="review-merge-now"
          onSelect={() => pickAndClose(() => { void review.mergePR({ method, deleteBranch, autoMerge: false }); })}
        />
        {#if armed}
          <MenuItem
            label="Cancel auto-merge"
            disabled={!armed.canCancel}
            testId="review-merge-cancel-auto"
            onSelect={() => pickAndClose(() => { void review.cancelPRAutoMerge(); })}
          />
        {:else}
          <MenuItem
            label="Auto-merge when ready"
            description={mergeState === 'ready' ? 'already ready' : !merge?.autoMergeAvailable ? 'not available here' : deleteBranch && review.prRef?.forge === 'github' ? 'GitHub keeps the branch' : undefined}
            disabled={!canAutoMerge}
            testId="review-merge-auto"
            onSelect={() => pickAndClose(() => { void review.mergePR({ method, deleteBranch, autoMerge: true }); })}
          />
        {/if}
      </div>
    </Menu>
  </Popover>
{/if}
