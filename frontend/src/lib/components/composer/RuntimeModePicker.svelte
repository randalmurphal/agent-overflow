<script lang="ts">
  import { fade, fly } from 'svelte/transition';
  import type { ThreadPane } from '../../stores/thread.svelte';
  import type { RuntimeMode } from '../../types/models';
  import { SetThreadRuntimeMode } from '../../stores/bindings';
  import { replaceThread } from '../../stores/threads.svelte';
  import { addToast } from '../../stores/toast.svelte';

  let { pane }: { pane: ThreadPane } = $props();

  let open = $state(false);
  let applying = $state(false);
  let triggerEl: HTMLButtonElement | undefined = $state(undefined);
  let listboxEl: HTMLDivElement | undefined = $state(undefined);

  // Back-compat: threads created before v12 of the schema may surface with
  // an undefined runtimeMode until the next reload. Normalize at the read
  // boundary rather than guessing at every call-site.
  const DEFAULT: RuntimeMode = 'full-access';
  let threadId = $derived(pane.thread?.id ?? '');
  let current: RuntimeMode = $derived(pane.thread?.runtimeMode ?? DEFAULT);

  interface ModeOption {
    mode: RuntimeMode;
    label: string;
    badge: string;
    description: string;
  }

  // Order is UX-intentional: safest first, least friction last. Matches
  // forge/t3-code presentation so users coming from those apps see the
  // same mental hierarchy.
  const OPTIONS: ModeOption[] = [
    {
      mode: 'approval-required',
      label: 'Approve each action',
      badge: 'Safe',
      description: 'Prompt for every tool use. Read-only sandbox on Codex.',
    },
    {
      mode: 'auto-accept-edits',
      label: 'Auto-accept edits',
      badge: 'Balanced',
      description: 'File edits inside the workspace run without prompts; commands still ask.',
    },
    {
      mode: 'full-access',
      label: 'Full access',
      badge: 'Default',
      description: 'No prompts. Danger-full-access sandbox on Codex.',
    },
  ];

  function badgeText(mode: RuntimeMode): string {
    return OPTIONS.find((o) => o.mode === mode)?.badge ?? 'Mode';
  }

  function openPicker() {
    open = true;
  }

  $effect(() => {
    if (open && listboxEl) {
      const target = listboxEl.querySelector<HTMLElement>(
        `button[role="option"][data-mode="${current}"]`,
      );
      (target ?? listboxEl.querySelector<HTMLElement>('button[role="option"]'))?.focus();
    }
  });

  async function selectMode(mode: RuntimeMode): Promise<void> {
    if (!threadId) return;
    if (mode === current) {
      open = false;
      triggerEl?.focus();
      return;
    }

    applying = true;
    open = false;
    triggerEl?.focus();
    try {
      const result = (await SetThreadRuntimeMode(threadId, mode)) as {
        threadId: string;
        runtimeMode: RuntimeMode;
        needsReconnect: boolean;
      };

      // Optimistic update: patch the pane's thread so the chip re-renders
      // immediately. The backend already persisted + (optionally) began a
      // reconnect; we don't wait for the async restart here.
      if (pane.thread) {
        const next = { ...pane.thread, runtimeMode: result.runtimeMode };
        pane.replaceThread(next);
        replaceThread(next);
      }

      if (result.needsReconnect) {
        addToast('info', `Runtime mode set to ${mode}. Reconnecting session…`);
      } else {
        addToast('info', `Runtime mode set to ${mode}.`);
      }
    } catch (err) {
      console.error('Failed to set runtime mode:', err);
      addToast('error', `Failed to set runtime mode: ${err}`);
    } finally {
      applying = false;
    }
  }

  function handleKeydown(e: KeyboardEvent) {
    if (e.key === 'Escape') {
      open = false;
      triggerEl?.focus();
    }
  }

  function handleBackdropClick(e: MouseEvent) {
    if (e.target === e.currentTarget) {
      open = false;
    }
  }
</script>

<div class="relative flex items-center min-w-0">
  <button
    bind:this={triggerEl}
    onclick={openPicker}
    disabled={applying}
    data-testid="runtime-mode-trigger"
    class="inline-flex items-center gap-1.5 max-w-[180px] truncate rounded-full border border-border px-2.5 py-1 text-[11px] text-text-secondary transition-colors hover:border-text-secondary hover:text-text-primary cursor-pointer focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/50 disabled:cursor-not-allowed disabled:opacity-60"
    aria-label="Change runtime mode"
    aria-expanded={open}
    aria-haspopup="listbox"
    title="Runtime mode — controls how much friction the agent's actions have before running."
  >
    <span
      class={[
        'inline-block h-1.5 w-1.5 rounded-full',
        current === 'approval-required' ? 'bg-emerald-400' : '',
        current === 'auto-accept-edits' ? 'bg-amber-400' : '',
        current === 'full-access' ? 'bg-rose-400' : '',
      ].join(' ')}
      aria-hidden="true"
    ></span>
    <span class="truncate">{applying ? 'Applying…' : badgeText(current)}</span>
  </button>

  {#if open}
    <!-- svelte-ignore a11y_no_static_element_interactions -->
    <div transition:fade={{ duration: 100 }} class="fixed inset-0 z-40" onclick={handleBackdropClick} onkeydown={handleKeydown}></div>
    <div
      bind:this={listboxEl}
      transition:fly={{ y: -4, duration: 120 }}
      class="absolute top-full left-0 mt-1 z-50 bg-surface-1 border border-border rounded-lg shadow-xl min-w-[260px]"
      role="listbox"
      aria-label="Runtime mode"
      data-testid="runtime-mode-listbox"
    >
      <div class="border-b border-border px-3 py-2.5">
        <p class="text-[10px] font-semibold uppercase tracking-[0.18em] text-text-secondary/70">
          Runtime mode
        </p>
        <p class="mt-1 text-[11px] leading-4 text-text-secondary/75">
          Changing mode on an active session restarts it so the provider picks up the new setting.
        </p>
      </div>
      {#each OPTIONS as opt (opt.mode)}
        <button
          onclick={() => selectMode(opt.mode)}
          role="option"
          aria-selected={opt.mode === current}
          data-mode={opt.mode}
          data-testid="runtime-mode-option-{opt.mode}"
          class={[
            'w-full text-left px-3 py-2 text-xs cursor-pointer flex flex-col gap-0.5 transition-colors',
            opt.mode === current
              ? 'bg-accent/10 text-text-primary'
              : 'hover:bg-surface-2/50 text-text-secondary hover:text-text-primary',
          ].join(' ')}
        >
          <span class="flex items-center gap-2">
            <span
              class={[
                'inline-block h-1.5 w-1.5 rounded-full',
                opt.mode === 'approval-required' ? 'bg-emerald-400' : '',
                opt.mode === 'auto-accept-edits' ? 'bg-amber-400' : '',
                opt.mode === 'full-access' ? 'bg-rose-400' : '',
              ].join(' ')}
              aria-hidden="true"
            ></span>
            <span class="font-medium">{opt.label}</span>
            {#if opt.mode === current}
              <span class="ml-auto text-accent" aria-hidden="true">&#10003;</span>
            {/if}
          </span>
          <span class="pl-3.5 text-[10px] text-text-secondary/75 leading-snug">{opt.description}</span>
        </button>
      {/each}
    </div>
  {/if}
</div>
