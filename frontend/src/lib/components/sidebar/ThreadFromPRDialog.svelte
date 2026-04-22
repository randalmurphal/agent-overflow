<script lang="ts">
  import Modal from '../primitives/Modal.svelte';
  import Button from '../primitives/Button.svelte';
  import { CreateThreadFromPR } from '../../stores/bindings';
  import { parsePRReference, type ParsedPRReference } from '../../utils/prReference';
  import { getSettings } from '../../stores/settings.svelte';
  import { addToast } from '../../stores/toast.svelte';
  import { prependThread } from '../../stores/threads.svelte';
  import type { Thread } from '../../types/models';
  import type { ThreadPane } from '../../stores/thread.svelte';

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
    // Focus routing is handled by Modal's focusTrap action; [data-autofocus]
    // on the URL input picks it as the initial focus target.
  });

  // Enter-to-submit keybinding when focus is inside the body fields.
  // Escape is handled by Modal's backdrop keydown listener.
  function handleBodyKeydown(e: KeyboardEvent) {
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
      onClose();
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

  const FIELD_CLASS =
    'w-full text-[13px] rounded-[var(--radius-control)] border border-border-subtle bg-surface-0 px-3 py-1.5 ' +
    'text-fg placeholder:text-fg-hint focus:outline-none focus:border-accent focus-visible:ring-2 focus-visible:ring-accent/40 ' +
    'transition-colors';
</script>

<Modal
  {open}
  title="Start thread from PR"
  onClose={onClose}
  width="lg"
  padding="comfortable"
>
  {#snippet children()}
    <!-- svelte-ignore a11y_no_static_element_interactions -->
    <div
      class="space-y-4"
      data-testid="thread-from-pr-dialog"
      onkeydown={handleBodyKeydown}
    >
      <div class="space-y-2">
        <label for="pr-url-input" class="text-[12px] text-fg-muted block font-medium">
          GitHub PR URL or <code class="text-[10px] bg-surface-2/60 px-1 rounded font-mono">OWNER/REPO#N</code>
        </label>
        <input
          id="pr-url-input"
          data-autofocus
          data-testid="thread-from-pr-url"
          type="text"
          bind:value={url}
          placeholder="https://github.com/owner/repo/pull/123"
          class={FIELD_CLASS}
        />
        {#if parseErrorMessage}
          <p class="text-[12px] text-error" role="alert" data-testid="thread-from-pr-parse-error">{parseErrorMessage}</p>
        {/if}
      </div>

      <div class="space-y-2">
        <span class="text-[12px] text-fg-muted block font-medium">Provider</span>
        <div class="flex gap-1" role="radiogroup" aria-label="Provider">
          {#each (['claude', 'codex'] as const) as choice}
            <button
              type="button"
              role="radio"
              aria-checked={provider === choice}
              data-testid={`thread-from-pr-provider-${choice}`}
              onclick={() => (provider = choice)}
              class={[
                'flex-1 text-[12px] py-1.5 rounded-[var(--radius-control)] cursor-pointer focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/40 transition-colors',
                provider === choice
                  ? 'bg-accent text-surface-0 font-medium'
                  : 'bg-surface-2/40 text-fg-muted hover:text-fg hover:bg-surface-2/60',
              ].join(' ')}
            >
              {choice === 'claude' ? 'Claude' : 'Codex'}
            </button>
          {/each}
        </div>
      </div>

      <div class="space-y-1">
        <label for="pr-model-input" class="text-[12px] text-fg-muted block font-medium">Model (optional)</label>
        <input
          id="pr-model-input"
          type="text"
          bind:value={model}
          placeholder={defaultModel ? `Model (default: ${defaultModel})` : 'Model (optional)'}
          class={FIELD_CLASS}
        />
      </div>

      {#if error}
        <p class="text-[12px] text-error break-words" role="alert" data-testid="thread-from-pr-error">{error}</p>
      {/if}
    </div>
  {/snippet}
  {#snippet footer()}
    <Button variant="secondary" size="sm" onclick={onClose}>
      {#snippet children()}Cancel{/snippet}
    </Button>
    <Button
      variant="primary"
      size="sm"
      testId="thread-from-pr-submit"
      onclick={() => void handleSubmit()}
      disabled={!canSubmit}
      loading={submitting}
    >
      {#snippet children()}{submitting ? 'Creating…' : 'Create'}{/snippet}
    </Button>
  {/snippet}
</Modal>
