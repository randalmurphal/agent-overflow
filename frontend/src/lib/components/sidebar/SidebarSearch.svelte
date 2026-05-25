<script lang="ts">
  // Sidebar search input. Drives the shared threadFilter store so the
  // command palette can imperatively focus us even when this component
  // isn't the active focus. The shortcut pill on the right is a live hint —
  // it reads the configured chord for `sidebar.focus-search` so user
  // rebinds in Settings flow through without a reload.

  import Search from 'lucide-svelte/icons/search';
  import X from 'lucide-svelte/icons/x';
  import {
    getThreadFilterQuery,
    setThreadFilterQuery,
  } from '../../stores/threadFilter.svelte';
  import { formatChord, keybindingForCommand } from '../../stores/keybindings.svelte';
  import Icon from '../primitives/Icon.svelte';
  import Kbd from '../primitives/Kbd.svelte';

  interface Props {
    /** Receives a focus callback the palette / keybindings can call. */
    registerFocusSearch?: (focus: () => void) => void;
  }

  let { registerFocusSearch }: Props = $props();

  let searchEl: HTMLInputElement | undefined = $state(undefined);
  let query = $derived(getThreadFilterQuery());
  let searchShortcut = $derived(
    formatChord(keybindingForCommand('sidebar.focus-search') ?? 'mod+/'),
  );

  $effect(() => {
    if (registerFocusSearch && searchEl) {
      registerFocusSearch(() => searchEl?.focus());
    }
  });

  function handleInput(e: Event): void {
    setThreadFilterQuery((e.target as HTMLInputElement).value);
  }

  function handleClear(): void {
    setThreadFilterQuery('');
    searchEl?.focus();
  }
</script>

<div class="px-3 pt-3 pb-2">
  <div class="relative">
    <span
      class="pointer-events-none absolute left-2.5 top-1/2 -translate-y-1/2 flex items-center text-fg-hint"
      aria-hidden="true"
    >
      <Icon icon={Search} size={13} strokeWidth={2} class="opacity-70" />
    </span>
    <input
      bind:this={searchEl}
      type="search"
      value={query}
      oninput={handleInput}
      placeholder="Search Projects & Threads…"
      aria-label="Search Projects and Threads"
      data-testid="sidebar-thread-search"
      class="w-full rounded-[var(--radius-control)] border border-border-subtle bg-surface-0/60 pl-8 pr-14 py-1.5 text-[0.75rem] text-fg placeholder:text-fg-hint focus:outline-none focus:border-border focus-visible:ring-2 focus-visible:ring-accent/40 transition-colors"
    />
    {#if query.length > 0}
      <button
        type="button"
        onclick={handleClear}
        aria-label="Clear Search"
        data-testid="sidebar-thread-search-clear"
        class="absolute right-2 top-1/2 -translate-y-1/2 flex h-5 w-5 items-center justify-center rounded-[var(--radius-field)] text-fg-subtle hover:text-fg hover:bg-surface-2/40 cursor-pointer focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/40 transition-colors"
      >
        <Icon icon={X} size={12} strokeWidth={2.5} class="opacity-90" />
      </button>
    {:else}
      <span
        class="absolute right-2 top-1/2 -translate-y-1/2 pointer-events-none select-none"
        aria-hidden="true"
        data-testid="sidebar-thread-search-kbd"
      >
        <Kbd>{searchShortcut}</Kbd>
      </span>
    {/if}
  </div>
</div>
