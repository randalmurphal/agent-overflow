<script lang="ts">
  import type { ThreadPane } from '../../stores/thread.svelte';
  import { GetPayloadData, WriteThreadWorkspaceFile } from '../../stores/bindings';
  import { addToast } from '../../stores/toast.svelte';
  import { copyToClipboard } from '../../utils/clipboard';
  import Modal from '../primitives/Modal.svelte';
  import Button from '../primitives/Button.svelte';
  import type { ProposedPlanMeta } from '../../types/models';
  import {
    buildProposedPlanMarkdownFilename,
    downloadPlanAsTextFile,
    normalizePlanMarkdownForExport,
    stripDisplayedPlanMarkdown,
  } from '../../utils/proposedPlan';
  import ChatMarkdown from './ChatMarkdown.svelte';

  let { pane, payloadId, meta }: { pane: ThreadPane; payloadId: string; meta: ProposedPlanMeta } = $props();

  let expanded = $state(false);
  let planMarkdown = $state<string | null>(null);
  let loading = $state(false);
  let saveDialogOpen = $state(false);
  let savePath = $state('');
  let saving = $state(false);
  let copied = $state(false);
  let copyTimer: ReturnType<typeof setTimeout> | undefined;

  const title = $derived(meta.title || 'Proposed plan');
  const canCollapse = $derived(meta.charCount > 900 || meta.lineCount > 20);
  const displayedMarkdown = $derived.by(() => {
    const source = planMarkdown ?? meta.preview;
    return planMarkdown ? stripDisplayedPlanMarkdown(source) : source;
  });

  async function ensurePlanMarkdown(): Promise<string> {
    const threadId = pane.threadId;
    if (planMarkdown !== null) {
      return planMarkdown;
    }
    if (loading) {
      return '';
    }
    if (!threadId) {
      addToast('error', 'Failed to load proposed plan');
      return '';
    }
    loading = true;
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
    }
  }

  async function handleToggleExpanded() {
    if (!expanded) {
      const fullPlan = await ensurePlanMarkdown();
      if (!fullPlan) return;
    }
    expanded = !expanded;
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

  async function handleDownload() {
    const fullPlan = await ensurePlanMarkdown();
    if (!fullPlan) return;
    downloadPlanAsTextFile(
      buildProposedPlanMarkdownFilename(fullPlan),
      normalizePlanMarkdownForExport(fullPlan)
    );
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

  $effect(() => {
    if (!canCollapse && planMarkdown === null) {
      void ensurePlanMarkdown();
    }
  });
</script>

<div class="mb-3 rounded-[24px] border border-border bg-surface-1/90 p-4 sm:p-5">
  <div class="flex flex-wrap items-center justify-between gap-3">
    <div class="flex min-w-0 items-center gap-2">
      <span class="rounded-full bg-accent/15 px-2 py-0.5 text-[11px] font-semibold uppercase tracking-wide text-accent">
        Plan
      </span>
      <p class="truncate text-sm font-medium text-text-primary">{title}</p>
    </div>
    <div class="flex items-center gap-1.5 text-xs text-text-secondary">
      <button
        onclick={handleCopy}
        class="rounded-md border border-border px-2.5 py-1 hover:text-text-primary hover:border-text-secondary cursor-pointer focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/50"
      >
        {copied ? 'Copied!' : 'Copy'}
      </button>
      <button
        onclick={handleDownload}
        class="rounded-md border border-border px-2.5 py-1 hover:text-text-primary hover:border-text-secondary cursor-pointer focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/50"
      >
        Download
      </button>
      <button
        onclick={openSaveDialog}
        class="rounded-md border border-border px-2.5 py-1 hover:text-text-primary hover:border-text-secondary cursor-pointer focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/50"
      >
        Save
      </button>
    </div>
  </div>

  <div class="mt-4">
    <div class:overflow-hidden={canCollapse && !expanded} class:max-h-104={canCollapse && !expanded} class="relative">
      <ChatMarkdown source={displayedMarkdown} />
      {#if canCollapse && !expanded}
        <div class="pointer-events-none absolute inset-x-0 bottom-0 h-24 bg-linear-to-t from-surface-1 via-surface-1/80 to-transparent"></div>
      {/if}
    </div>
    {#if canCollapse}
      <div class="mt-4 flex justify-center">
        <button
          onclick={handleToggleExpanded}
          class="rounded-md border border-border px-3 py-1.5 text-sm text-text-secondary hover:text-text-primary hover:border-text-secondary cursor-pointer focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/50"
        >
          {expanded ? 'Collapse plan' : 'Expand plan'}
        </button>
      </div>
    {/if}
  </div>
</div>

<Modal
  open={saveDialogOpen}
  title="Save Plan to Workspace"
  onClose={closeSaveDialog}
  width="lg"
  padding="comfortable"
>
  {#snippet children()}
    <p class="text-[13px] text-fg-muted mb-4 leading-relaxed">
      Enter a path relative to <code class="font-mono text-[12px] bg-surface-2/50 px-1 rounded">{pane.thread?.workspacePath ?? 'the workspace'}</code>.
    </p>

    <label class="block">
      <span class="mb-1 block text-[12px] text-fg-muted font-medium">Workspace Path</span>
      <input
        data-autofocus
        bind:value={savePath}
        disabled={saving}
        spellcheck={false}
        placeholder="plans/my-plan.md"
        class="w-full text-[13px] rounded-[var(--radius-control)] border border-border-subtle bg-surface-0 px-3 py-1.5 text-fg placeholder:text-fg-hint focus:outline-none focus:border-accent focus-visible:ring-2 focus-visible:ring-accent/40 transition-colors"
      />
    </label>
  {/snippet}
  {#snippet footer()}
    <Button
      variant="secondary"
      size="sm"
      onclick={closeSaveDialog}
      disabled={saving}
    >
      {#snippet children()}Cancel{/snippet}
    </Button>
    <Button
      variant="primary"
      size="sm"
      onclick={handleSave}
      loading={saving}
    >
      {#snippet children()}{saving ? 'Saving…' : 'Save'}{/snippet}
    </Button>
  {/snippet}
</Modal>
