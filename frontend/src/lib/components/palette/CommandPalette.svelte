<script lang="ts">
  // Command palette. Routes backdrop + focus-trap + Escape + role=dialog
  // through the Modal primitive (top-aligned, no body padding, no
  // default header) so we don't reimplement any of that here. The
  // custom header snippet renders the search input instead of the
  // default title chrome, and the body is the filtered results list.

  import { tick } from 'svelte';
  import { closePalette, isPaletteOpen } from '../../stores/palette.svelte';
  import { enabledCommands, type Command, type CommandContext } from '../../stores/commandRegistry.svelte';
  import { fuzzyFilter } from '../../utils/fuzzy';
  import { formatChord, keybindingForCommand } from '../../stores/keybindings.svelte';
  import Modal from '../primitives/Modal.svelte';
  import PaletteResultRow from './PaletteResultRow.svelte';

  let {
    context,
    contextForPane,
  }: {
    context: CommandContext;
    contextForPane?: (paneId: string) => CommandContext | null;
  } = $props();

  let query = $state('');
  let activeIndex = $state(0);
  let inputEl: HTMLInputElement | undefined = $state(undefined);
  let listEl: HTMLDivElement | undefined = $state(undefined);

  let open = $derived(isPaletteOpen());
  let targetPaneId: string | null = $state(null);
  let wasOpen = false;

  // Every time the palette opens, reset state and focus the textbox.
  // Modal's focusTrap doesn't autofocus the input by default (no
  // [data-autofocus]) so we explicitly focus it after mount.
  $effect(() => {
    if (open && !wasOpen) {
      targetPaneId = context.paneId;
      query = '';
      activeIndex = 0;
      tick().then(() => inputEl?.focus());
    } else if (!open && wasOpen) {
      targetPaneId = null;
    }
    wasOpen = open;
  });

  interface PaletteRow {
    command: Command;
    shortcut: string | null;
    indices: number[];
  }

  let activeContext = $derived(
    targetPaneId && contextForPane ? contextForPane(targetPaneId) : context,
  );
  let allCommands = $derived(activeContext ? enabledCommands(activeContext) : []);

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
    // Escape is handled by Modal's backdrop-level keydown; don't
    // double-fire here or we'd call closePalette twice.
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
    const capturedContext = activeContext;
    if (!capturedContext) {
      closePalette();
      return;
    }
    closePalette();
    // Run after close so the palette stops capturing focus before the
    // command mutates the app (e.g. opening settings).
    queueMicrotask(() => {
      void Promise.resolve(row.command.run(capturedContext)).catch((err) => {
        console.error(`command ${row.command.id} failed:`, err);
      });
    });
  }

  let activeId = $derived(results[activeIndex]?.command.id);
</script>

<Modal
  {open}
  onClose={closePalette}
  ariaLabel="Command palette"
  width="md"
  padding="none"
  align="top"
>
  {#snippet header()}
    <div class="px-4 pt-3.5 pb-2 border-b border-border-subtle">
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
        placeholder="Type a command…"
        class="w-full bg-transparent text-[15px] text-fg placeholder:text-fg-hint outline-none"
        data-testid="command-palette-input"
      />
    </div>
  {/snippet}
  {#snippet children()}
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
        <div class="px-4 py-6 text-center text-[13px] text-fg-subtle" data-testid="command-palette-empty">
          No commands match "{query}".
        </div>
      {/if}
    </div>
  {/snippet}
</Modal>
