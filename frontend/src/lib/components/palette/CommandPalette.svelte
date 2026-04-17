<script lang="ts">
  import { tick } from 'svelte';
  import { closePalette, isPaletteOpen } from '../../stores/palette.svelte';
  import { enabledCommands, type Command, type CommandContext } from '../../stores/commandRegistry.svelte';
  import { fuzzyFilter } from '../../utils/fuzzy';
  import { formatChord, keybindingForCommand } from '../../stores/keybindings.svelte';
  import PaletteResultRow from './PaletteResultRow.svelte';

  let {
    context,
  }: {
    context: CommandContext;
  } = $props();

  let query = $state('');
  let activeIndex = $state(0);
  let inputEl: HTMLInputElement | undefined = $state(undefined);
  let listEl: HTMLDivElement | undefined = $state(undefined);

  let open = $derived(isPaletteOpen());

  // Every time the palette opens, reset state and focus the textbox.
  $effect(() => {
    if (open) {
      query = '';
      activeIndex = 0;
      tick().then(() => inputEl?.focus());
    }
  });

  interface PaletteRow {
    command: Command;
    shortcut: string | null;
    indices: number[];
  }

  let allCommands = $derived(enabledCommands(context));

  let results: PaletteRow[] = $derived.by(() => {
    const candidates = allCommands.map((cmd) => ({ item: cmd, text: cmd.label }));
    const matches = fuzzyFilter(query.trim(), candidates);
    return matches.map((m) => ({
      command: m.item,
      shortcut: shortcutFor(m.item.id),
      indices: m.indices,
    }));
  });

  function shortcutFor(id: string): string | null {
    const raw = keybindingForCommand(id);
    if (!raw) return null;
    return formatChord(raw);
  }

  $effect(() => {
    // Clamp active index when results shrink.
    if (activeIndex >= results.length) activeIndex = Math.max(0, results.length - 1);
  });

  function handleInputKeydown(ev: KeyboardEvent): void {
    if (ev.key === 'Escape') {
      ev.preventDefault();
      closePalette();
      return;
    }
    if (ev.key === 'ArrowDown') {
      ev.preventDefault();
      if (results.length === 0) return;
      activeIndex = (activeIndex + 1) % results.length;
      scrollActiveIntoView();
      return;
    }
    if (ev.key === 'ArrowUp') {
      ev.preventDefault();
      if (results.length === 0) return;
      activeIndex = (activeIndex - 1 + results.length) % results.length;
      scrollActiveIntoView();
      return;
    }
    if (ev.key === 'Home') {
      ev.preventDefault();
      activeIndex = 0;
      scrollActiveIntoView();
      return;
    }
    if (ev.key === 'End') {
      ev.preventDefault();
      activeIndex = Math.max(0, results.length - 1);
      scrollActiveIntoView();
      return;
    }
    if (ev.key === 'Enter') {
      ev.preventDefault();
      executeActive();
      return;
    }
  }

  function scrollActiveIntoView(): void {
    tick().then(() => {
      const active = results[activeIndex];
      if (!active || !listEl) return;
      const el = listEl.querySelector<HTMLElement>(`#palette-option-${cssEscape(active.command.id)}`);
      el?.scrollIntoView({ block: 'nearest' });
    });
  }

  function cssEscape(value: string): string {
    if (typeof CSS !== 'undefined' && typeof CSS.escape === 'function') return CSS.escape(value);
    return value.replace(/[^a-zA-Z0-9_-]/g, '\\$&');
  }

  function executeActive(): void {
    const row = results[activeIndex];
    if (!row) return;
    closePalette();
    // Run after close so the palette stops capturing focus before the
    // command mutates the app (e.g. opening settings).
    queueMicrotask(() => {
      void Promise.resolve(row.command.run(context)).catch((err) => {
        console.error(`command ${row.command.id} failed:`, err);
      });
    });
  }

  function handleBackdropClick(ev: MouseEvent): void {
    if (ev.target === ev.currentTarget) closePalette();
  }

  function handleBackdropKeydown(ev: KeyboardEvent): void {
    if (ev.key === 'Escape') {
      ev.preventDefault();
      closePalette();
    }
  }

  let activeId = $derived(results[activeIndex]?.command.id);
</script>

{#if open}
  <!-- svelte-ignore a11y_click_events_have_key_events -->
  <!-- svelte-ignore a11y_no_static_element_interactions -->
  <div
    class="fixed inset-0 z-[70] flex items-start justify-center pt-[10vh] bg-overlay backdrop-blur-sm"
    onclick={handleBackdropClick}
    onkeydown={handleBackdropKeydown}
    data-testid="command-palette-backdrop"
  >
    <div
      role="dialog"
      aria-modal="true"
      aria-label="Command palette"
      class="w-[560px] max-w-[calc(100vw-2rem)] rounded-2xl border border-border/70 bg-surface-1/95 shadow-[0_40px_80px_-30px_rgba(0,0,0,0.6)] overflow-hidden flex flex-col"
    >
      <div class="px-4 pt-4 pb-2 border-b border-border/60">
        <input
          bind:this={inputEl}
          bind:value={query}
          onkeydown={handleInputKeydown}
          type="text"
          role="combobox"
          aria-expanded="true"
          aria-controls="palette-listbox"
          aria-autocomplete="list"
          aria-activedescendant={activeId ? `palette-option-${activeId}` : undefined}
          placeholder="Type a command..."
          class="w-full bg-transparent text-base text-text-primary placeholder:text-text-secondary/50 outline-none"
          data-testid="command-palette-input"
        />
      </div>
      <div
        bind:this={listEl}
        role="listbox"
        id="palette-listbox"
        class="max-h-[360px] overflow-y-auto py-1"
        aria-label="Commands"
      >
        {#each results as row, idx (row.command.id)}
          <PaletteResultRow
            command={row.command}
            shortcut={row.shortcut}
            selected={idx === activeIndex}
            matchIndices={row.indices}
            onMouseEnter={() => (activeIndex = idx)}
            onClick={() => {
              activeIndex = idx;
              executeActive();
            }}
          />
        {/each}
        {#if results.length === 0}
          <div class="px-4 py-6 text-center text-sm text-text-secondary/70" data-testid="command-palette-empty">
            No commands match "{query}".
          </div>
        {/if}
      </div>
    </div>
  </div>
{/if}
