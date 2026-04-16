<script lang="ts">
  import { fade, scale } from 'svelte/transition';
  import type { ThreadPane } from '../../stores/thread.svelte';
  import type { GitActionResult } from '../../types/git';
  import { GitCommit } from '../../stores/bindings';
  import { addToast } from '../../stores/toast.svelte';

  let { pane, open, onClose }: {
    pane: ThreadPane;
    open: boolean;
    onClose: () => void;
  } = $props();

  let subject = $state('');
  let body = $state('');
  let committing = $state(false);
  let error = $state<string | null>(null);
  let dialogEl: HTMLDivElement | undefined = $state(undefined);
  let previousFocus: Element | null = null;

  async function handleCommit() {
    if (!subject.trim() || !pane.threadId || committing) return;
    committing = true;
    error = null;
    try {
      const result = await GitCommit(pane.threadId, subject.trim(), body.trim());
      const r = result as GitActionResult;
      if (r.error) {
        error = r.error;
      } else {
        addToast('success', `Committed ${r.commitSha?.slice(0, 7) ?? ''}`);
        subject = '';
        body = '';
        close();
      }
    } catch (err) {
      error = err instanceof Error ? err.message : String(err);
    } finally {
      committing = false;
    }
  }

  function handleKeydown(e: KeyboardEvent) {
    if (e.key === 'Escape') {
      e.preventDefault();
      close();
      return;
    }
    if (e.key === 'Tab' && dialogEl) {
      const focusable = dialogEl.querySelectorAll<HTMLElement>(
        'input:not([disabled]), textarea:not([disabled]), button:not([disabled]), [tabindex]:not([tabindex="-1"])',
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

  function close() {
    if (previousFocus instanceof HTMLElement) {
      previousFocus.focus();
    }
    previousFocus = null;
    onClose();
  }

  function handleBackdropClick(e: MouseEvent) {
    if (e.target === e.currentTarget) {
      close();
    }
  }

  $effect(() => {
    if (open && dialogEl) {
      previousFocus = document.activeElement;
      const subjectInput = dialogEl.querySelector<HTMLElement>('#commit-subject');
      subjectInput?.focus();
    }
  });
</script>

{#if open}
  <!-- svelte-ignore a11y_no_static_element_interactions -->
  <div
    transition:fade={{ duration: 150 }}
    class="fixed inset-0 z-[60] flex items-center justify-center bg-overlay backdrop-blur-sm"
    onclick={handleBackdropClick}
    onkeydown={handleKeydown}
  >
    <div
      bind:this={dialogEl}
      transition:scale={{ start: 0.95, duration: 150 }}
      role="dialog"
      aria-modal="true"
      aria-labelledby="commit-dialog-title"
      class="bg-surface-1 border border-border rounded-lg shadow-xl max-w-lg w-full mx-4 p-5"
    >
      <h2 id="commit-dialog-title" class="text-base font-semibold text-text-primary mb-4">Commit Changes</h2>

      <div class="space-y-3">
        <div>
          <label for="commit-subject" class="text-xs text-text-secondary block mb-1">Subject</label>
          <input
            id="commit-subject"
            type="text"
            bind:value={subject}
            maxlength={72}
            placeholder="Brief description of changes"
            class="w-full text-sm rounded border border-border bg-surface-0 px-3 py-2 text-text-primary placeholder:text-text-secondary/40 focus:outline-none focus:border-accent focus-visible:ring-2 focus-visible:ring-accent/50 transition-colors"
          />
          <span class="text-[10px] text-text-secondary/40 mt-0.5 block text-right">{subject.length}/72</span>
        </div>

        <div>
          <label for="commit-body" class="text-xs text-text-secondary block mb-1">Body (optional)</label>
          <textarea
            id="commit-body"
            bind:value={body}
            rows={4}
            placeholder="Extended description..."
            class="w-full text-sm rounded border border-border bg-surface-0 px-3 py-2 text-text-primary placeholder:text-text-secondary/40 focus:outline-none focus:border-accent resize-none"
          ></textarea>
        </div>

        {#if error}
          <p class="text-xs text-error break-words">{error}</p>
        {/if}
      </div>

      <div class="flex justify-end gap-2 mt-5">
        <button
          onclick={close}
          class="px-4 py-2 text-sm rounded-md border border-border text-text-secondary hover:text-text-primary cursor-pointer focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/50"
        >
          Cancel
        </button>
        <button
          onclick={handleCommit}
          disabled={!subject.trim() || committing}
          class="px-4 py-2 text-sm rounded-md font-medium bg-accent text-surface-0 hover:opacity-90 disabled:opacity-40 disabled:cursor-not-allowed cursor-pointer focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/50"
        >
          {committing ? 'Committing...' : 'Commit'}
        </button>
      </div>
    </div>
  </div>
{/if}
