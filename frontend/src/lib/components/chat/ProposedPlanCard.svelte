<script lang="ts">
  import { fade, scale } from 'svelte/transition';
  import type { ThreadPane } from '../../stores/thread.svelte';
  import { GetPayloadData, HighlightMarkdown, WriteThreadWorkspaceFile } from '../../stores/bindings';
  import { addToast } from '../../stores/toast.svelte';
  import { copyToClipboard } from '../../utils/clipboard';
  import type { ProposedPlanMeta } from '../../types/models';
  import {
    buildProposedPlanMarkdownFilename,
    downloadPlanAsTextFile,
    normalizePlanMarkdownForExport,
    stripDisplayedPlanMarkdown,
  } from '../../utils/proposedPlan';

  let { pane, payloadId, meta }: { pane: ThreadPane; payloadId: string; meta: ProposedPlanMeta } = $props();

  let expanded = $state(false);
  let planMarkdown = $state<string | null>(null);
  let loading = $state(false);
  let saveDialogOpen = $state(false);
  let savePath = $state('');
  let saving = $state(false);
  let copied = $state(false);
  let copyTimer: ReturnType<typeof setTimeout> | undefined;
  let dialogEl: HTMLDivElement | undefined = $state(undefined);
  let previousFocus: Element | null = null;
  const dialogId = crypto.randomUUID().slice(0, 8);

  const title = $derived(meta.title || 'Proposed plan');
  const canCollapse = $derived(meta.charCount > 900 || meta.lineCount > 20);
  const displayedMarkdown = $derived.by(() => {
    const source = planMarkdown ?? meta.preview;
    return planMarkdown ? stripDisplayedPlanMarkdown(source) : source;
  });

  async function ensurePlanMarkdown(): Promise<string> {
    if (planMarkdown !== null) {
      return planMarkdown;
    }
    if (loading) {
      return '';
    }
    loading = true;
    try {
      const content = await GetPayloadData(payloadId);
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

  // ProposedPlanCard applies markdown-level transforms
  // (stripDisplayedPlanMarkdown strips the title heading; the collapsed
  // path would truncate) BEFORE rendering, so we can't reuse the
  // pre-rendered HTML returned by GetPayloadData. Render on demand via
  // the HighlightMarkdown binding, memoised by the exact markdown we
  // hand it so flipping expanded ↔ collapsed hits the same cache key
  // when the underlying source hasn't changed.
  let displayedHtml = $state<string>('');
  let lastRenderedSource = '';
  $effect(() => {
    const source = displayedMarkdown;
    if (source === lastRenderedSource) return;
    lastRenderedSource = source;
    if (!source) {
      displayedHtml = '';
      return;
    }
    HighlightMarkdown(source).then((html) => {
      if (lastRenderedSource === source) {
        displayedHtml = html;
      }
    });
  });

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
    if (previousFocus instanceof HTMLElement) {
      previousFocus.focus();
    }
    previousFocus = null;
    saveDialogOpen = false;
  }

  function handleDialogBackdropClick(e: MouseEvent) {
    if (e.target === e.currentTarget && !saving) {
      closeSaveDialog();
    }
  }

  function handleDialogKeydown(e: KeyboardEvent) {
    if (e.key === 'Escape' && !saving) {
      e.preventDefault();
      closeSaveDialog();
      return;
    }
    if (e.key === 'Tab' && dialogEl) {
      const focusable = dialogEl.querySelectorAll<HTMLElement>(
        'input:not([disabled]), button:not([disabled]), [tabindex]:not([tabindex="-1"])',
      );
      if (focusable.length === 0) return;
      const first = focusable[0];
      const last = focusable[focusable.length - 1];
      if (e.shiftKey && document.activeElement === first) {
        e.preventDefault();
        last.focus();
      } else if (!e.shiftKey && document.activeElement === last) {
        e.preventDefault();
        first.focus();
      }
    }
  }

  $effect(() => {
    if (!canCollapse && planMarkdown === null) {
      void ensurePlanMarkdown();
    }
  });

  $effect(() => {
    if (saveDialogOpen && dialogEl) {
      previousFocus = document.activeElement;
      const input = dialogEl.querySelector<HTMLInputElement>('input');
      input?.focus();
      input?.select();
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
      <div class="markdown-body">{@html displayedHtml}</div>
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

{#if saveDialogOpen}
  <!-- svelte-ignore a11y_no_static_element_interactions -->
  <div
    transition:fade={{ duration: 150 }}
    class="fixed inset-0 z-[60] flex items-center justify-center bg-overlay backdrop-blur-sm"
    onclick={handleDialogBackdropClick}
    onkeydown={handleDialogKeydown}
  >
    <div
      bind:this={dialogEl}
      transition:scale={{ start: 0.95, duration: 150 }}
      role="dialog"
      aria-modal="true"
      aria-labelledby="plan-save-title-{dialogId}"
      aria-describedby="plan-save-desc-{dialogId}"
      class="bg-surface-1 border border-border rounded-lg shadow-xl max-w-lg w-full mx-4 p-5"
    >
      <h2 id="plan-save-title-{dialogId}" class="text-base font-semibold text-text-primary mb-1.5">
        Save plan to workspace
      </h2>
      <p id="plan-save-desc-{dialogId}" class="text-sm text-text-secondary mb-4">
        Enter a path relative to <code>{pane.thread?.workspacePath ?? 'the workspace'}</code>.
      </p>

      <label class="block">
        <span class="mb-1 block text-xs text-text-secondary">Workspace path</span>
        <input
          bind:value={savePath}
          disabled={saving}
          spellcheck={false}
          placeholder="plans/my-plan.md"
          class="w-full text-sm rounded border border-border bg-surface-0 px-3 py-2 text-text-primary placeholder:text-text-secondary/40 focus:outline-none focus:border-accent focus-visible:ring-2 focus-visible:ring-accent/50 transition-colors"
        />
      </label>

      <div class="mt-5 flex justify-end gap-2">
        <button
          onclick={closeSaveDialog}
          disabled={saving}
          class="px-4 py-2 text-sm rounded-md border border-border text-text-secondary hover:text-text-primary cursor-pointer disabled:opacity-40 disabled:cursor-not-allowed focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/50"
        >
          Cancel
        </button>
        <button
          onclick={handleSave}
          disabled={saving}
          class="px-4 py-2 text-sm rounded-md font-medium bg-accent text-surface-0 hover:opacity-90 disabled:opacity-40 disabled:cursor-not-allowed cursor-pointer focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/50"
        >
          {saving ? 'Saving...' : 'Save'}
        </button>
      </div>
    </div>
  </div>
{/if}
