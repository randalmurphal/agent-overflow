<script lang="ts">
  // Sidebar search input. Drives the shared threadFilter store so the
  // command palette can imperatively focus us even when this component
  // isn't the active focus. The ⌘K pill on the right is a hint only —
  // the real Cmd/Ctrl+K keybinding is registered globally in App.svelte.

  import {
    getThreadFilterQuery,
    setThreadFilterQuery,
  } from '../../stores/threadFilter.svelte';

  interface Props {
    /** Receives a focus callback the palette / keybindings can call. */
    registerFocusSearch?: (focus: () => void) => void;
  }

  let { registerFocusSearch }: Props = $props();

  let searchEl: HTMLInputElement | undefined = $state(undefined);
  let query = $derived(getThreadFilterQuery());

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

<div class="px-3 py-3 border-b border-border/60">
  <div class="relative">
    <svg
      class="pointer-events-none absolute left-3 top-1/2 -translate-y-1/2 h-3.5 w-3.5 text-text-secondary/60"
      viewBox="0 0 24 24"
      fill="none"
      stroke="currentColor"
      stroke-width="2"
      stroke-linecap="round"
      stroke-linejoin="round"
      aria-hidden="true"
    >
      <circle cx="11" cy="11" r="7" />
      <path d="m21 21-4.3-4.3" />
    </svg>
    <input
      bind:this={searchEl}
      type="search"
      value={query}
      oninput={handleInput}
      placeholder="Search projects & threads..."
      aria-label="Search projects and threads"
      data-testid="sidebar-thread-search"
      class="w-full rounded-xl border border-border/60 bg-surface-0/70 pl-9 pr-16 py-1.5 text-xs text-text-primary placeholder:text-text-secondary/50 focus:outline-none focus:border-accent focus-visible:ring-2 focus-visible:ring-accent/50 transition-colors"
    />
    {#if query.length > 0}
      <button
        type="button"
        onclick={handleClear}
        aria-label="Clear search"
        data-testid="sidebar-thread-search-clear"
        class="absolute right-2 top-1/2 -translate-y-1/2 flex h-5 w-5 items-center justify-center rounded text-text-secondary/70 hover:text-text-primary hover:bg-surface-2/60 cursor-pointer focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/50"
      >
        <svg
          class="h-3 w-3"
          viewBox="0 0 24 24"
          fill="none"
          stroke="currentColor"
          stroke-width="2.5"
          stroke-linecap="round"
          stroke-linejoin="round"
          aria-hidden="true"
        >
          <path d="M18 6 6 18" />
          <path d="m6 6 12 12" />
        </svg>
      </button>
    {:else}
      <span
        class="absolute right-2 top-1/2 -translate-y-1/2 rounded border border-border/60 bg-surface-2/40 px-1.5 py-0.5 text-[10px] font-medium tracking-wide text-text-secondary/70 pointer-events-none select-none"
        aria-hidden="true"
        data-testid="sidebar-thread-search-kbd"
      >
        ⌘K
      </span>
    {/if}
  </div>
</div>
