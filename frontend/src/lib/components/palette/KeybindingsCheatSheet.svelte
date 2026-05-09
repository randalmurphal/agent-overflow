<script lang="ts">
  import Modal from '../primitives/Modal.svelte';
  import Button from '../primitives/Button.svelte';
  import Kbd from '../primitives/Kbd.svelte';
  import { listCommands, type Command } from '../../stores/commandRegistry.svelte';
  import { formatChord, keybindingForCommand } from '../../stores/keybindings.svelte';

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

</script>

<Modal
  {open}
  title="Keyboard shortcuts"
  onClose={onClose}
  width="lg"
  padding="tight"
  align="top"
>
  {#snippet children()}
    <div data-testid="keybindings-cheatsheet">
      <p class="text-[12px] text-fg-muted mb-3">Customize these in Settings → Keybindings.</p>
      <input
        bind:this={searchEl}
        bind:value={searchQuery}
        type="text"
        placeholder="Search commands or shortcuts…"
        aria-label="Search shortcuts"
        data-testid="keybindings-cheatsheet-search"
        class="w-full text-[13px] rounded-[var(--radius-control)] border border-border-subtle bg-surface-0 px-3 py-1.5 text-fg placeholder:text-fg-hint focus:outline-none focus:border-accent focus-visible:ring-2 focus-visible:ring-accent/40 transition-colors mb-3"
      />

      <div class="space-y-4">
        {#if filteredRows.length === 0}
          <p class="text-[12px] italic text-fg-muted" data-testid="keybindings-cheatsheet-empty">
            No commands match "{searchQuery}".
          </p>
        {:else}
          {#each grouped as group (group.category)}
            <section aria-labelledby="cheatsheet-group-{group.category}">
              <h3
                id="cheatsheet-group-{group.category}"
                class="text-[10px] uppercase tracking-[0.18em] text-fg-subtle font-semibold mb-1.5"
                data-testid="keybindings-cheatsheet-group-{group.category}"
              >
                {group.category}
              </h3>
              <ul class="space-y-0.5">
                {#each group.rows as row (row.command.id)}
                  <li
                    class="flex items-start justify-between gap-3 py-1.5 px-2 rounded-[var(--radius-field)] hover:bg-surface-2/30 transition-colors"
                    data-testid="keybindings-cheatsheet-row-{row.command.id}"
                  >
                    <div class="min-w-0 flex-1">
                      <p class="text-[13px] text-fg">{row.command.label}</p>
                      {#if row.command.description}
                        <p class="text-[11px] text-fg-muted mt-0.5">{row.command.description}</p>
                      {/if}
                      <p class="text-[10px] font-mono text-fg-hint mt-0.5">{row.command.id}</p>
                    </div>
                    <div class="shrink-0">
                      {#if row.chord}
                        <span
                          data-testid="keybindings-cheatsheet-chord-{row.command.id}"
                          title={row.chord}
                        >
                          <Kbd>{formatChord(row.chord)}</Kbd>
                        </span>
                      {:else}
                        <span
                          class="text-[10px] italic text-fg-hint"
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
    </div>
  {/snippet}
  {#snippet footer()}
    <Button
      variant="secondary"
      size="sm"
      onclick={onClose}
      testId="keybindings-cheatsheet-close"
    >
      {#snippet children()}Close{/snippet}
    </Button>
  {/snippet}
</Modal>
