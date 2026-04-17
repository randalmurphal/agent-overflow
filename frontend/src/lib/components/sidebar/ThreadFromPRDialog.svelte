<script lang="ts">
  import { fade, scale } from 'svelte/transition';
  import { CreateThreadFromPR } from '../../stores/bindings';
  import { parsePRReference, type ParsedPRReference } from '../../utils/prReference';
  import { getSettings } from '../../stores/settings.svelte';
  import { addToast } from '../../stores/toast.svelte';
  import { prependThread } from '../../stores/threads.svelte';
  import type { Thread } from '../../types/models';
  import type { ThreadPane } from '../../stores/thread.svelte';
  import { focusTrap } from '../../utils/focusTrap';
  import ProviderPicker from '../composer/ProviderPicker.svelte';

  let { open, pane, onClose }: {
    open: boolean;
    pane: ThreadPane;
    onClose: () => void;
  } = $props();

  let url = $state('');
  let provider = $state<'claude' | 'codex'>(getSettings().defaultProvider as 'claude' | 'codex');
  let model = $state('');
  let submitting = $state(false);
  let error = $state<string | null>(null);
  let dialogEl: HTMLDivElement | undefined = $state(undefined);
  // Monotonic counter bumped whenever the dialog opens or closes. Each
  // submission captures this at start; if the user closes the dialog
  // before CreateThreadFromPR resolves the counter has moved, so we
  // don't switch the pane or flash a toast on a dialog they dismissed.
  let submitGeneration = 0;

  // Live validation feedback — computed each render so the "Create" button's
  // disabled state tracks exactly what the submit path would see.
  let parsed = $derived.by<{ ok: true; value: ParsedPRReference } | { ok: false; error: string } | null>(() => {
    if (url.trim() === '') return null;
    return parsePRReference(url);
  });
  let parseErrorMessage = $derived(parsed && !parsed.ok ? parsed.error : null);
  let canSubmit = $derived(
    !submitting && parsed !== null && parsed.ok && provider !== null,
  );
  let defaultModel = $derived(
    provider === 'claude' ? getSettings().defaultModelClaude : getSettings().defaultModelCodex,
  );

  $effect(() => {
    if (!open) {
      // Dialog was closed; bump so any in-flight submission bails.
      submitGeneration += 1;
      return;
    }
    // Dialog is opening — bump once so a fresh submission starts in a new
    // generation window.
    submitGeneration += 1;
    url = '';
    model = '';
    error = null;
    submitting = false;
    provider = getSettings().defaultProvider as 'claude' | 'codex';
    // Focus routing is handled by the focusTrap action; [data-autofocus]
    // on the URL input picks it as the initial focus target.
  });

  function close() {
    // focus restoration is handled by the focusTrap action.
    onClose();
  }

  function handleBackdropClick(e: MouseEvent) {
    if (e.target === e.currentTarget) close();
  }

  function handleKeydown(e: KeyboardEvent) {
    if (e.key === 'Escape') {
      e.preventDefault();
      close();
      return;
    }
    if (e.key === 'Enter' && !e.shiftKey && canSubmit) {
      e.preventDefault();
      void handleSubmit();
    }
  }

  async function handleSubmit(): Promise<void> {
    if (!canSubmit || parsed === null || !parsed.ok) return;
    submitting = true;
    error = null;
    const effectiveModel = model.trim() || defaultModel;
    const startGeneration = submitGeneration;
    const prNumber = parsed.value.number;
    try {
      const ownerRepo = `${parsed.value.owner}/${parsed.value.repo}`;
      const thread = (await CreateThreadFromPR(
        ownerRepo,
        prNumber,
        provider,
        effectiveModel,
      )) as Thread;
      if (submitGeneration !== startGeneration) {
        // User closed the dialog before the backend finished. The thread
        // exists server-side but we must not navigate away or pollute
        // the already-dismissed dialog's state. Keep them informed with
        // a non-disruptive toast so they know where the thread went.
        addToast('info', `Thread from PR #${prNumber} was created in the background`);
        return;
      }
      prependThread(thread);
      await pane.switchThread(thread);
      addToast('success', `Thread created from PR #${prNumber}`);
      close();
    } catch (err) {
      if (submitGeneration !== startGeneration) {
        // Dialog is gone — don't paint an error onto a dismissed UI.
        console.error('CreateThreadFromPR failed after dialog dismissed:', err);
        return;
      }
      console.error('CreateThreadFromPR failed:', err);
      error = err instanceof Error ? err.message : String(err);
    } finally {
      if (submitGeneration === startGeneration) {
        submitting = false;
      }
    }
  }
</script>

{#if open}
  <!-- svelte-ignore a11y_no_static_element_interactions -->
  <div
    transition:fade={{ duration: 150 }}
    class="fixed inset-0 z-[60] flex items-center justify-center bg-overlay backdrop-blur-sm"
    data-testid="thread-from-pr-backdrop"
    onclick={handleBackdropClick}
    onkeydown={handleKeydown}
  >
    <div
      bind:this={dialogEl}
      use:focusTrap={{ active: open }}
      transition:scale={{ start: 0.95, duration: 150 }}
      role="dialog"
      aria-modal="true"
      aria-labelledby="thread-from-pr-title"
      data-testid="thread-from-pr-dialog"
      class="bg-surface-1 border border-border rounded-lg shadow-xl max-w-lg w-full mx-4 p-5 space-y-4"
    >
      <h2 id="thread-from-pr-title" class="text-base font-semibold text-text-primary">
        Start thread from PR
      </h2>

      <div class="space-y-2">
        <label for="pr-url-input" class="text-xs text-text-secondary block">
          GitHub PR URL or <code class="text-[10px] bg-surface-2/60 px-1 rounded">OWNER/REPO#N</code>
        </label>
        <input
          id="pr-url-input"
          data-autofocus
          data-testid="thread-from-pr-url"
          type="text"
          bind:value={url}
          placeholder="https://github.com/owner/repo/pull/123"
          class="w-full text-sm rounded border border-border bg-surface-0 px-3 py-2 text-text-primary placeholder:text-text-secondary/40 focus:outline-none focus:border-accent focus-visible:ring-2 focus-visible:ring-accent/50 transition-colors"
        />
        {#if parseErrorMessage}
          <p class="text-xs text-error" role="alert" data-testid="thread-from-pr-parse-error">{parseErrorMessage}</p>
        {/if}
      </div>

      <div class="space-y-2">
        <span class="text-xs text-text-secondary block">Provider</span>
        <ProviderPicker currentProvider={provider} onSelect={(p) => provider = p as 'claude' | 'codex'} />
      </div>

      <div class="space-y-1">
        <label for="pr-model-input" class="text-xs text-text-secondary block">Model (optional)</label>
        <input
          id="pr-model-input"
          type="text"
          bind:value={model}
          placeholder={defaultModel ? `Model (default: ${defaultModel})` : 'Model (optional)'}
          class="w-full text-xs rounded border border-border bg-surface-0 px-3 py-2 text-text-primary placeholder:text-text-secondary/40 focus:outline-none focus:border-accent focus-visible:ring-2 focus-visible:ring-accent/50 transition-colors"
        />
      </div>

      {#if error}
        <p class="text-xs text-error break-words" role="alert" data-testid="thread-from-pr-error">{error}</p>
      {/if}

      <div class="flex justify-end gap-2 pt-1">
        <button
          type="button"
          onclick={close}
          class="px-4 py-2 text-sm rounded-md border border-border text-text-secondary hover:text-text-primary cursor-pointer focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/50"
        >
          Cancel
        </button>
        <button
          type="button"
          data-testid="thread-from-pr-submit"
          onclick={() => void handleSubmit()}
          disabled={!canSubmit}
          class="px-4 py-2 text-sm rounded-md font-medium bg-accent text-surface-0 hover:opacity-90 disabled:opacity-40 disabled:cursor-not-allowed cursor-pointer focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/50"
        >
          {submitting ? 'Creating…' : 'Create'}
        </button>
      </div>
    </div>
  </div>
{/if}
