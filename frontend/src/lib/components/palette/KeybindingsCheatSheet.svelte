<script lang="ts">
  import { fade, scale } from 'svelte/transition';
  import { focusTrap } from '../../utils/focusTrap';
  import { listCommands, type Command } from '../../stores/commandRegistry.svelte';
  import { keybindingForCommand } from '../../stores/keybindings.svelte';

  interface Props {
    open: boolean;
    onClose: () => void;
  }

  let { open, onClose }: Props = $props();

  let searchQuery = $state('');
  let searchEl: HTMLInputElement | undefined = $state(undefined);

  // Reset search on each open and park focus in the search box.
  $effect(() => {
    if (open) {
      searchQuery = '';
      // Defer focus to after the transition so the element exists.
      requestAnimationFrame(() => {
        searchEl?.focus();
      });
    }
  });

  interface Row {
    command: Command;
    chord: string | null;
    category: string;
  }

  function categorize(commandId: string): string {
    const dot = commandId.indexOf('.');
    if (dot < 0) return 'General';
    const prefix = commandId.slice(0, dot);
    // Capitalize for display: "thread" → "Thread".
    return prefix.charAt(0).toUpperCase() + prefix.slice(1);
  }

  // Snapshot commands + their bindings. Reading the store directly gives us
  // reactivity — listCommands and keybindingForCommand are both derived from
  // $state-backed stores.
  let allRows = $derived.by<Row[]>(() => {
    const cmds = listCommands();
    const rows: Row[] = cmds.map((command) => ({
      command,
      chord: keybindingForCommand(command.id),
      category: categorize(command.id),
    }));
    // Alphabetize within each category so similar commands cluster.
    rows.sort((a, b) => {
      if (a.category !== b.category) return a.category.localeCompare(b.category);
      return a.command.label.localeCompare(b.command.label);
    });
    return rows;
  });

  let filteredRows = $derived.by<Row[]>(() => {
    const q = searchQuery.trim().toLowerCase();
    if (!q) return allRows;
    return allRows.filter((r) => {
      if (r.command.label.toLowerCase().includes(q)) return true;
      if (r.command.id.toLowerCase().includes(q)) return true;
      if (r.command.description?.toLowerCase().includes(q)) return true;
      if (r.chord && r.chord.toLowerCase().includes(q)) return true;
      return false;
    });
  });

  // Group filtered rows by category for display.
  let grouped = $derived.by<Array<{ category: string; rows: Row[] }>>(() => {
    const map = new Map<string, Row[]>();
    for (const row of filteredRows) {
      const list = map.get(row.category) ?? [];
      list.push(row);
      map.set(row.category, list);
    }
    return Array.from(map.entries())
      .map(([category, rows]) => ({ category, rows }))
      .sort((a, b) => a.category.localeCompare(b.category));
  });

  function handleKeydown(e: KeyboardEvent) {
    if (e.key === 'Escape') {
      e.preventDefault();
      onClose();
    }
  }

  function handleBackdropClick(e: MouseEvent) {
    if (e.target === e.currentTarget) onClose();
  }

  // Pretty-print a chord: "mod+k" → "⌘K" on Mac, "Ctrl+K" otherwise. Kept
  // trivial — the raw chord string is already readable enough for most users,
  // and we surface both for discoverability.
  function displayChord(chord: string): string {
    const isMac = typeof navigator !== 'undefined' && /Mac|iPhone|iPad/.test(navigator.platform);
    return chord
      .split('+')
      .map((part) => {
        const p = part.trim().toLowerCase();
        if (p === 'mod') return isMac ? '⌘' : 'Ctrl';
        if (p === 'ctrl') return 'Ctrl';
        if (p === 'shift') return isMac ? '⇧' : 'Shift';
        if (p === 'alt') return isMac ? '⌥' : 'Alt';
        if (p === 'meta') return isMac ? '⌘' : 'Meta';
        if (p.length === 1) return p.toUpperCase();
        return part.trim();
      })
      .join(isMac ? '' : ' + ');
  }
</script>

{#if open}
  <!-- svelte-ignore a11y_no_static_element_interactions -->
  <div
    transition:fade={{ duration: 150 }}
    class="fixed inset-0 z-[60] flex items-start justify-center pt-20 bg-overlay backdrop-blur-sm"
    onclick={handleBackdropClick}
    onkeydown={handleKeydown}
    data-testid="keybindings-cheatsheet-backdrop"
  >
    <div
      use:focusTrap={{ active: open }}
      transition:scale={{ start: 0.95, duration: 150 }}
      role="dialog"
      aria-modal="true"
      aria-labelledby="keybindings-cheatsheet-title"
      data-testid="keybindings-cheatsheet"
      class="bg-surface-1 border border-border rounded-lg shadow-xl max-w-2xl w-full mx-4 max-h-[80vh] flex flex-col"
    >
      <div class="px-5 pt-5 pb-3 border-b border-border">
        <h2 id="keybindings-cheatsheet-title" class="text-base font-semibold text-text-primary">
          Keyboard shortcuts
        </h2>
        <p class="text-xs text-text-secondary mt-0.5">
          Customize these in Settings → Keybindings.
        </p>
        <input
          bind:this={searchEl}
          bind:value={searchQuery}
          type="text"
          placeholder="Search commands or shortcuts…"
          aria-label="Search shortcuts"
          data-testid="keybindings-cheatsheet-search"
          class="mt-3 w-full text-sm rounded border border-border bg-surface-0 px-2.5 py-1.5 text-text-primary focus:outline-none focus:ring-2 focus:ring-accent/50"
        />
      </div>

      <div class="flex-1 overflow-y-auto px-5 py-3 space-y-4">
        {#if filteredRows.length === 0}
          <p class="text-xs italic text-text-secondary" data-testid="keybindings-cheatsheet-empty">
            No commands match "{searchQuery}".
          </p>
        {:else}
          {#each grouped as group (group.category)}
            <section aria-labelledby="cheatsheet-group-{group.category}">
              <h3
                id="cheatsheet-group-{group.category}"
                class="text-[10px] uppercase tracking-wide text-text-secondary/70 mb-1.5"
                data-testid="keybindings-cheatsheet-group-{group.category}"
              >
                {group.category}
              </h3>
              <ul class="space-y-1">
                {#each group.rows as row (row.command.id)}
                  <li
                    class="flex items-start justify-between gap-3 py-1.5 px-2 rounded hover:bg-surface-2/50"
                    data-testid="keybindings-cheatsheet-row-{row.command.id}"
                  >
                    <div class="min-w-0 flex-1">
                      <p class="text-sm text-text-primary">{row.command.label}</p>
                      {#if row.command.description}
                        <p class="text-[10px] text-text-secondary/80 mt-0.5">{row.command.description}</p>
                      {/if}
                      <p class="text-[10px] font-mono text-text-secondary/60 mt-0.5">{row.command.id}</p>
                    </div>
                    <div class="shrink-0">
                      {#if row.chord}
                        <kbd
                          class="text-[11px] font-mono px-1.5 py-0.5 rounded bg-surface-0 border border-border text-text-primary"
                          data-testid="keybindings-cheatsheet-chord-{row.command.id}"
                          title={row.chord}
                        >
                          {displayChord(row.chord)}
                        </kbd>
                      {:else}
                        <span
                          class="text-[10px] italic text-text-secondary/60"
                          data-testid="keybindings-cheatsheet-unbound-{row.command.id}"
                        >
                          unbound
                        </span>
                      {/if}
                    </div>
                  </li>
                {/each}
              </ul>
            </section>
          {/each}
        {/if}
      </div>

      <div class="flex justify-end gap-2 px-5 py-3 border-t border-border">
        <button
          type="button"
          data-testid="keybindings-cheatsheet-close"
          onclick={onClose}
          class="px-3 py-1.5 text-xs rounded-md border border-border text-text-secondary hover:text-text-primary hover:border-text-secondary cursor-pointer focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/50"
        >
          Close
        </button>
      </div>
    </div>
  </div>
{/if}
