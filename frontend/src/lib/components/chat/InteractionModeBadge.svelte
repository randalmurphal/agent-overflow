<script lang="ts">
  import { fade, fly } from 'svelte/transition';
  import { SetThreadInteractionMode } from '../../stores/bindings';
  import type { ThreadPane } from '../../stores/thread.svelte';
  import type { Thread } from '../../types/models';
  import { replaceThread } from '../../stores/threads.svelte';
  import { addToast } from '../../stores/toast.svelte';

  let { pane }: { pane: ThreadPane } = $props();

  type ModeOption = {
    value: Thread['interactionMode'];
    label: string;
    description: string;
  };

  // "discussion" is intentionally omitted from the picker — discussion threads
  // are created through the StartDiscussion flow, which sets up a deliberation
  // channel. Switching into discussion mode here would produce a broken thread
  // with no participants; if a user wants a discussion they should start one.
  const modeOptions: ModeOption[] = [
    { value: 'default', label: 'Default', description: 'Normal coding turns' },
    { value: 'plan', label: 'Plan', description: 'Propose a plan before acting' },
    { value: 'design', label: 'Design', description: 'Interactive design artifacts' },
  ];

  let current = $derived(pane.thread?.interactionMode ?? 'default');
  let open = $state(false);
  let updating = $state(false);
  let triggerEl: HTMLButtonElement | undefined = $state(undefined);
  let menuEl: HTMLDivElement | undefined = $state(undefined);

  let label = $derived(modeOptions.find((o) => o.value === current)?.label ?? 'Discussion');

  $effect(() => {
    if (open && menuEl) {
      const first = menuEl.querySelector<HTMLElement>('button[role="menuitemradio"]');
      first?.focus();
    }
  });

  async function pickMode(mode: Thread['interactionMode']): Promise<void> {
    open = false;
    triggerEl?.focus();
    const threadId = pane.threadId;
    if (!threadId || updating || mode === current) return;
    // Resolve the label from the parameter, not from the derived `label`.
    // The derived tracks `current` which only updates after
    // pane.replaceThread() below, and Svelte's reactive propagation is
    // lazy enough that reading `label` right after the await can still
    // see the pre-update value in some scheduler configurations — the
    // audit spotted a "Mode set to Default" toast after switching into
    // Plan. Resolving the label up front removes the timing coupling.
    const nextLabel =
      modeOptions.find((o) => o.value === mode)?.label ?? String(mode);
    updating = true;
    try {
      const updated = (await SetThreadInteractionMode(threadId, mode)) as Thread;
      pane.replaceThread(updated);
      replaceThread(updated);
      addToast('info', `Mode set to ${nextLabel}`);
    } catch (err) {
      console.error('Failed to set interaction mode:', err);
      pane.setError(`Failed to set interaction mode: ${err instanceof Error ? err.message : String(err)}`);
    } finally {
      updating = false;
    }
  }

  function handleMenuKeydown(e: KeyboardEvent) {
    if (e.key === 'Escape') {
      open = false;
      triggerEl?.focus();
      return;
    }
    if (e.key === 'ArrowDown' || e.key === 'ArrowUp') {
      e.preventDefault();
      if (!menuEl) return;
      const items = [...menuEl.querySelectorAll<HTMLElement>('button[role="menuitemradio"]')];
      if (items.length === 0) return;
      const idx = items.indexOf(document.activeElement as HTMLElement);
      const next = e.key === 'ArrowDown'
        ? (idx + 1) % items.length
        : (idx - 1 + items.length) % items.length;
      items[next].focus();
    }
  }

  function handleBackdropClick(e: MouseEvent) {
    if (e.target === e.currentTarget) {
      open = false;
    }
  }
</script>

{#if pane.thread}
  <div class="relative">
    <button
      bind:this={triggerEl}
      type="button"
      onclick={() => { open = !open; }}
      aria-haspopup="menu"
      aria-expanded={open}
      aria-label={`Interaction mode: ${label}. Click to change.`}
      title={`Interaction mode: ${label}`}
      data-testid="interaction-mode-badge"
      disabled={updating}
      class="text-[10px] font-bold px-1.5 py-0.5 rounded border cursor-pointer focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/50 disabled:opacity-40
        {current === 'design'
          ? 'border-provider-codex/40 bg-provider-codex/15 text-provider-codex/90'
          : current === 'plan'
            ? 'border-accent/40 bg-accent/15 text-accent'
            : current === 'discussion'
              ? 'border-border/50 bg-surface-2/60 text-text-secondary'
              : 'border-border/50 bg-surface-2/40 text-text-secondary hover:text-text-primary'}"
    >
      {label.toUpperCase()}
    </button>
    {#if open}
      <!-- svelte-ignore a11y_no_static_element_interactions -->
      <div
        transition:fade={{ duration: 100 }}
        class="fixed inset-0 z-40"
        onclick={handleBackdropClick}
        onkeydown={(e) => { if (e.key === 'Escape') { open = false; triggerEl?.focus(); } }}
      ></div>
      <!-- svelte-ignore a11y_interactive_supports_focus -->
      <div
        bind:this={menuEl}
        onkeydown={handleMenuKeydown}
        role="menu"
        aria-label="Change interaction mode"
        transition:fly={{ y: -4, duration: 120 }}
        class="absolute top-full left-0 mt-1 z-50 bg-surface-1 border border-border rounded-lg shadow-lg min-w-[200px] py-1"
      >
        {#each modeOptions as opt (opt.value)}
          <button
            type="button"
            role="menuitemradio"
            aria-checked={current === opt.value}
            data-testid={`interaction-mode-option-${opt.value}`}
            onclick={() => pickMode(opt.value)}
            class="w-full text-left px-3 py-1.5 text-xs hover:bg-surface-2/50 cursor-pointer flex flex-col focus-visible:outline-none focus-visible:bg-surface-2/50
              {current === opt.value ? 'text-accent font-medium' : 'text-text-secondary'}"
          >
            <span class="flex items-center gap-1.5">
              {#if current === opt.value}
                <span aria-hidden="true" class="text-accent">✓</span>
              {:else}
                <span aria-hidden="true" class="w-3"></span>
              {/if}
              <span>{opt.label}</span>
            </span>
            <span class="ml-4 text-[10px] text-text-secondary/70">{opt.description}</span>
          </button>
        {/each}
      </div>
    {/if}
  </div>
{/if}
