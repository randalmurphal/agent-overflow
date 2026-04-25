<script lang="ts">
  // DesignOptionsPicker — when an agent blocks on present_options, show each
  // option as a card. Selecting a card previews it in DesignPreviewPanel.
  // Confirming a pick resolves the pending request via ChooseDesignOption.
  //
  // On success the backend emits `design:chosen`, which the event router uses
  // to clear pane.pendingDesignOptions. We don't clear locally — that would
  // leave the pane open to stale state if the backend reports a failure.

  import type { ThreadPane } from '../../stores/thread.svelte';
  import { ChooseDesignOption } from '../../stores/bindings';
  import { addToast } from '../../stores/toast.svelte';
  import Button from '../primitives/Button.svelte';

  let { pane }: { pane: ThreadPane } = $props();

  let request = $derived(pane.pendingDesignOptions);
  let selectedOptionId: string | null = $state(null);
  let submitting: boolean = $state(false);

  // When a new request arrives, default-select its first option so the user
  // sees something in the preview without having to click first.
  $effect(() => {
    const r = request;
    if (r && r.options.length > 0 && !selectedOptionId) {
      selectedOptionId = r.options[0].id;
    }
    if (!r) {
      selectedOptionId = null;
      submitting = false;
    }
  });

  // Selecting an option updates the pane-level activeArtifactId so the
  // preview iframe follows the picker.
  $effect(() => {
    const r = request;
    if (!r) return;
    const option = r.options.find((o) => o.id === selectedOptionId);
    if (option) pane.setActiveArtifact(option.artifactId);
  });

  function handleSelect(optionId: string) {
    if (submitting) return;
    selectedOptionId = optionId;
  }

  async function handleConfirm() {
    const r = request;
    if (!r || submitting || !selectedOptionId) return;
    const threadId = pane.threadId;
    if (!threadId) return;

    submitting = true;
    try {
      await ChooseDesignOption(threadId, r.requestId, selectedOptionId);
      // Don't clear pendingDesignOptions here — the backend emits
      // `design:chosen` which the event router handles.
    } catch (err: unknown) {
      const message = err instanceof Error ? err.message : String(err);
      addToast('error', `Failed to choose design option: ${message}`);
      submitting = false;
    }
  }
</script>

{#if request}
  <div class="border-t border-border-subtle bg-transparent p-3 shrink-0 flex flex-col gap-2">
    <div class="flex items-baseline gap-2">
      <h3 class="text-[13px] font-medium text-fg">Pick a Design Direction</h3>
      <span class="text-[10px] text-fg-hint font-mono" title="Request ID (debug)">
        req {request.requestId.slice(0, 8)}
      </span>
    </div>
    {#if request.prompt}
      <p class="text-[12px] text-fg-muted leading-relaxed">{request.prompt}</p>
    {/if}
    <div class="flex flex-wrap gap-2">
      {#each request.options as option (option.id)}
        {@const isSelected = option.id === selectedOptionId}
        <button
          type="button"
          aria-pressed={isSelected}
          aria-label="Select Design Option: {option.title}"
          disabled={submitting}
          onclick={() => handleSelect(option.id)}
          class="flex-1 min-w-[140px] text-left rounded-[var(--radius-control)] border px-3 py-2 transition-colors cursor-pointer focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/40 disabled:cursor-not-allowed
            {isSelected
              ? 'border-accent/70 bg-accent/10 text-fg'
              : 'border-border-subtle bg-card/30 text-fg-muted hover:bg-surface-2/30 hover:text-fg'}"
        >
          <div class="text-[12px] font-semibold truncate">{option.title}</div>
          {#if option.description}
            <div class="text-[11px] text-fg-muted/80 mt-0.5 line-clamp-2">{option.description}</div>
          {/if}
        </button>
      {/each}
    </div>
    <div class="flex justify-end">
      <Button
        variant="primary"
        size="sm"
        onclick={handleConfirm}
        disabled={!selectedOptionId}
        loading={submitting}
      >
        {#snippet children()}{submitting ? 'Choosing…' : 'Choose this option'}{/snippet}
      </Button>
    </div>
  </div>
{/if}
