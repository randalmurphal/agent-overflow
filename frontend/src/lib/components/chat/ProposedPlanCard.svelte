<script lang="ts">
  import type { ThreadPane } from '../../stores/thread.svelte';
  import {
    GetPayloadData,
    ListProposedPlanComments,
    SendPlanRevisionComments,
    WriteThreadWorkspaceFile,
  } from '../../stores/bindings';
  import { replaceThread } from '../../stores/threads.svelte';
  import { addToast } from '../../stores/toast.svelte';
  import { copyToClipboard } from '../../utils/clipboard';
  import { implementProposedPlan } from '../../utils/proposedPlanImplementation';
  import type { Item, ProposedPlanComment, ProposedPlanMeta, SourceProposedPlan, Thread } from '../../types/models';
  import {
    buildProposedPlanMarkdownFilename,
    normalizePlanMarkdownForExport,
    parseProposedPlanItemMeta,
    sourceFromProposedPlanItem,
    stripDisplayedPlanMarkdown,
  } from '../../utils/proposedPlan';
  import ProposedPlanActions from './ProposedPlanActions.svelte';
  import ProposedPlanBody from './ProposedPlanBody.svelte';
  import ProposedPlanReviewSurface from './ProposedPlanReviewSurface.svelte';
  import ProposedPlanSaveModal from './ProposedPlanSaveModal.svelte';

  let {
    pane,
    item,
    payloadId,
    meta,
    showReview = false,
    fullPlan = false,
  }: {
    pane: ThreadPane;
    item?: Item;
    payloadId: string;
    meta: ProposedPlanMeta;
    showReview?: boolean;
    fullPlan?: boolean;
  } = $props();

  let planMarkdown = $state<string | null>(null);
  let loading = $state(false);
  let implementing = $state(false);
  let saveDialogOpen = $state(false);
  let savePath = $state('');
  let saving = $state(false);
  let copied = $state(false);
  let comments: ProposedPlanComment[] = $state([]);
  let planMarkdownRequest: Promise<string> | null = null;
  let copyTimer: ReturnType<typeof setTimeout> | undefined;

  const title = $derived(meta.title || 'Proposed plan');
  const itemMeta = $derived(parseProposedPlanItemMeta(item));
  const planVersion = $derived(itemMeta.planVersion ?? 0);
  const isImplemented = $derived(Boolean(itemMeta.planImplementedAt));
  const previewOnly = $derived(!fullPlan && (meta.charCount > 900 || meta.lineCount > 20));
  const displayedMarkdown = $derived.by(() => {
    const source = planMarkdown ?? meta.preview;
    return planMarkdown ? stripDisplayedPlanMarkdown(source) : source;
  });

  async function ensurePlanMarkdown(): Promise<string> {
    const threadId = pane.threadId;
    if (planMarkdown !== null) {
      return planMarkdown;
    }
    if (planMarkdownRequest) return planMarkdownRequest;
    if (!threadId) {
      addToast('error', 'Failed to load proposed plan');
      return '';
    }
    loading = true;
    planMarkdownRequest = (async () => {
      try {
        const content = await GetPayloadData(threadId, payloadId);
        planMarkdown = content.data;
        return content.data;
      } catch (err) {
        console.error('Failed to load proposed plan:', err);
        addToast('error', 'Failed to load proposed plan');
        return '';
      } finally {
        loading = false;
        planMarkdownRequest = null;
      }
    })();
    return planMarkdownRequest;
  }

  async function refreshComments(): Promise<void> {
    if (!pane.threadId || !item?.id) {
      comments = [];
      return;
    }
    try {
      comments = ((await ListProposedPlanComments(pane.threadId, item.id)) as ProposedPlanComment[] | null) ?? [];
    } catch (err) {
      console.error('Failed to load plan comments:', err);
      comments = [];
    }
  }

  async function handleCopy() {
    const fullPlan = await ensurePlanMarkdown();
    if (!fullPlan) return;
    const ok = await copyToClipboard(normalizePlanMarkdownForExport(fullPlan));
    if (!ok) {
      addToast('error', 'Failed to copy plan');
      return;
    }
    copied = true;
    clearTimeout(copyTimer);
    copyTimer = setTimeout(() => {
      copied = false;
    }, 2000);
  }

  function sourceProposedPlan(): SourceProposedPlan | undefined {
    return sourceFromProposedPlanItem(pane.threadId, item) ?? undefined;
  }

  async function handleImplement() {
    if (!pane.threadId || implementing) return;
    const source = sourceProposedPlan();
    if (!source) return;
    implementing = true;
    try {
      await implementProposedPlan(pane, source, 'Failed to send implementation request');
    } finally {
      implementing = false;
    }
  }

  async function handleSendDraftComments(commentIds: string[]): Promise<void> {
    if (!pane.threadId || !item?.id) return;
    try {
      const updated = (await SendPlanRevisionComments(pane.threadId, item.id, commentIds)) as Thread;
      pane.replaceThread(updated);
      replaceThread(updated);
    } catch (err) {
      console.error('Failed to send plan comments:', err);
      addToast('error', 'Failed to send comments');
    }
  }

  async function openSaveDialog() {
    const fullPlan = await ensurePlanMarkdown();
    if (!fullPlan) return;
    savePath = savePath.trim() || buildProposedPlanMarkdownFilename(fullPlan);
    saveDialogOpen = true;
  }

  async function handleSave() {
    if (!pane.threadId || saving) return;
    const relativePath = savePath.trim();
    if (!relativePath) {
      addToast('warning', 'Enter a workspace-relative path');
      return;
    }
    const fullPlan = await ensurePlanMarkdown();
    if (!fullPlan) return;

    saving = true;
    try {
      const writtenPath = await WriteThreadWorkspaceFile(
        pane.threadId,
        relativePath,
        normalizePlanMarkdownForExport(fullPlan)
      );
      addToast('success', `Plan saved to ${writtenPath}`);
      closeSaveDialog();
    } catch (err) {
      console.error('Failed to save proposed plan:', err);
      addToast('error', err instanceof Error ? err.message : 'Failed to save plan');
    } finally {
      saving = false;
    }
  }

  function closeSaveDialog() {
    // Don't close mid-save — the RPC call is still in flight and the
    // wizard state machine expects `saveDialogOpen` to stay true until
    // the write resolves.
    if (saving) return;
    saveDialogOpen = false;
  }

  function updateSavePath(value: string) {
    savePath = value;
  }

  function openInSidebar(): void {
    if (!item?.id) return;
    pane.openPlanSidebarForItem(item.id);
  }

  $effect(() => {
    if ((showReview || fullPlan) && planMarkdown === null) {
      void ensurePlanMarkdown();
    }
  });

  $effect(() => {
    pane.threadId;
    item?.id;
    showReview;
    if (showReview) {
      void refreshComments();
    } else {
      comments = [];
    }
  });
</script>

<div class="mb-3 rounded-md border border-border bg-surface-1/90 p-4 sm:p-5">
  <div class="flex flex-wrap items-center justify-between gap-3">
    <div class="flex min-w-0 items-center gap-2">
      <span class="rounded bg-accent/15 px-2 py-0.5 text-[11px] font-semibold uppercase tracking-wide text-accent">
        {planVersion ? `Plan v${planVersion}` : 'Plan'}
      </span>
      <p class="truncate text-sm font-medium text-text-primary">{title}</p>
      {#if isImplemented}
        <span class="rounded bg-success/12 px-1.5 py-0.5 text-[10px] font-semibold uppercase tracking-wide text-success">
          Implemented
        </span>
      {/if}
    </div>
    <ProposedPlanActions
      {copied}
      implemented={isImplemented}
      {implementing}
      onImplement={handleImplement}
      onCopy={handleCopy}
      onSave={openSaveDialog}
      onOpenInSidebar={fullPlan ? undefined : openInSidebar}
    />
  </div>

  <ProposedPlanBody
    markdown={displayedMarkdown}
    {previewOnly}
  />

  {#if showReview && planMarkdown}
    <div class="mt-4">
      <ProposedPlanReviewSurface
        threadId={pane.threadId ?? ''}
        planItemId={item?.id ?? ''}
        markdown={normalizePlanMarkdownForExport(planMarkdown)}
        {comments}
        onRefresh={refreshComments}
        onSendDrafts={handleSendDraftComments}
      />
    </div>
  {/if}
</div>

<ProposedPlanSaveModal
  open={saveDialogOpen}
  workspacePath={pane.thread?.workspacePath}
  {savePath}
  {saving}
  onPathChange={updateSavePath}
  onClose={closeSaveDialog}
  onSave={handleSave}
/>
