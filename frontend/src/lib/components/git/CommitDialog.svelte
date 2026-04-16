<script lang="ts">
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
        onClose();
      }
    } catch (err) {
      error = err instanceof Error ? err.message : String(err);
    } finally {
      committing = false;
    }
  }

  function handleKeydown(e: KeyboardEvent) {
    if (e.key === 'Escape') {
      onClose();
    }
  }

  function handleBackdropClick(e: MouseEvent) {
    if (e.target === e.currentTarget) {
      onClose();
    }
  }
</script>

{#if open}
  <!-- svelte-ignore a11y_no_static_element_interactions -->
  <div
    class="fixed inset-0 z-50 flex items-center justify-center bg-black/60 backdrop-blur-sm"
    onclick={handleBackdropClick}
    onkeydown={handleKeydown}
  >
    <div class="bg-surface-1 border border-border rounded-lg shadow-xl max-w-lg w-full mx-4 p-5">
      <h2 class="text-base font-semibold text-text-primary mb-4">Commit Changes</h2>

      <div class="space-y-3">
        <div>
          <label for="commit-subject" class="text-xs text-text-secondary block mb-1">Subject</label>
          <input
            id="commit-subject"
            type="text"
            bind:value={subject}
            maxlength={72}
            placeholder="Brief description of changes"
            class="w-full text-sm rounded border border-border bg-surface-0 px-3 py-2 text-text-primary placeholder:text-text-secondary/40 focus:outline-none focus:border-accent"
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
          <p class="text-xs text-red-400">{error}</p>
        {/if}
      </div>

      <div class="flex justify-end gap-2 mt-5">
        <button
          onclick={onClose}
          class="px-4 py-2 text-sm rounded-md border border-border text-text-secondary hover:text-text-primary cursor-pointer"
        >
          Cancel
        </button>
        <button
          onclick={handleCommit}
          disabled={!subject.trim() || committing}
          class="px-4 py-2 text-sm rounded-md font-medium bg-accent text-surface-0 hover:opacity-90 disabled:opacity-40 disabled:cursor-not-allowed cursor-pointer"
        >
          {committing ? 'Committing...' : 'Commit'}
        </button>
      </div>
    </div>
  </div>
{/if}
