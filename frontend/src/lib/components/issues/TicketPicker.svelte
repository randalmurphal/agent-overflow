<script lang="ts" module>
  /** A value a picker offers. */
  export interface PickItem {
    readonly id: string;
    readonly name: string;
    readonly description?: string;
  }

  /** What a picker's read answered: the values, and whether the tracker
   * holds more matches than it listed. */
  export interface PickList {
    readonly items: readonly PickItem[];
    readonly more: boolean;
  }
</script>

<script lang="ts">
  // A ticket value's pick list, at its control: the values a read answers
  // (a move list held by the view, or a form read of the tracker), typed
  // text narrowing them with one read per pause in typing, and one row
  // that clears the value where the field may be empty. A read's failure
  // shows its sentence in the list. The values a field holds now are
  // checked; a pick closes the list unless the field holds several.
  import Menu from '../primitives/Menu.svelte';
  import MenuItem from '../primitives/MenuItem.svelte';
  import Popover from '../primitives/Popover.svelte';
  import { restorePickerFocus } from '../panes/paneComposerFocus';
  import type { PopoverCloseReason } from '../../utils/popoverOwnership';
  import { errString } from '../../utils/errors';

  interface Props {
    anchor: HTMLElement | undefined;
    open: boolean;
    label: string;
    /** Reads the values matching `text`. */
    load: (text: string) => Promise<PickList>;
    /** Typed text asks the tracker again; without it the list is filtered
     * here. */
    searchable: boolean;
    /** The IDs the field holds now. */
    checked: ReadonlySet<string>;
    /** The row that clears the value ("Unassigned"); none for a field
     * that must hold one. */
    clearLabel?: string;
    /** The field holds several values: a pick toggles one and the list
     * stays open. */
    multi?: boolean;
    onPick: (item: PickItem | null) => void;
    onClose: () => void;
  }

  let { anchor, open, label, load, searchable, checked, clearLabel, multi = false, onPick, onClose }: Props = $props();

  const PAUSE_MS = 250;

  let text = $state('');
  let list = $state.raw<PickList | null>(null);
  let failure = $state<string | null>(null);
  let reading = $state(false);
  // Only the last read asked lands.
  let asked = 0;
  let timer: ReturnType<typeof setTimeout> | null = null;

  async function read(query: string): Promise<void> {
    const mine = ++asked;
    reading = true;
    failure = null;
    try {
      const got = await load(query);
      if (mine === asked) list = got;
    } catch (err) {
      if (mine === asked) failure = errString(err);
    } finally {
      if (mine === asked) reading = false;
    }
  }

  $effect(() => {
    if (!open) return;
    text = '';
    list = null;
    void read('');
    return () => {
      asked += 1;
      if (timer) clearTimeout(timer);
      timer = null;
    };
  });

  function onInput(): void {
    if (!searchable) return;
    if (timer) clearTimeout(timer);
    const query = text;
    timer = setTimeout(() => {
      timer = null;
      void read(query);
    }, PAUSE_MS);
  }

  const shown = $derived.by(() => {
    const items = list?.items ?? [];
    if (searchable) return items;
    const words = text.trim().toLowerCase();
    return words ? items.filter((i) => i.name.toLowerCase().includes(words)) : items;
  });

  function close(reason?: PopoverCloseReason): void {
    onClose();
    restorePickerFocus(reason, { triggerEl: anchor });
  }

  function pick(item: PickItem | null): void {
    onPick(item);
    if (!multi) close();
  }
</script>

<Popover {anchor} {open} onClose={close} placement="bottom-start" ariaLabel={label}>
  <div class="flex max-h-80 w-64 flex-col">
    <input
      class="m-1.5 rounded-[var(--radius-field)] border border-border-subtle bg-surface-0 px-2 py-1 text-sm text-fg focus:outline-none focus-visible:ring-2 focus-visible:ring-accent/40"
      placeholder="Find"
      aria-label="Find {label.toLowerCase()}"
      data-testid="ticket-picker-find"
      bind:value={text}
      oninput={onInput}
    />
    <div class="min-h-0 overflow-y-auto">
      <Menu ariaLabel={label} onClose={close} minWidthClass="min-w-0">
        {#if clearLabel}
          <MenuItem label={clearLabel} checked={checked.size === 0} onSelect={() => pick(null)} testId="ticket-picker-clear" />
        {/if}
        {#each shown as item (item.id)}
          <MenuItem label={item.name} description={item.description} checked={checked.has(item.id)} checkbox={multi} onSelect={() => pick(item)} testId="ticket-picker-item" />
        {/each}
      </Menu>
      {#if failure}
        <p class="px-3 py-2 text-xs text-error" role="alert" data-testid="ticket-picker-error">{failure}</p>
      {:else if reading && !list}
        <p class="px-3 py-2 text-xs text-fg-muted" data-testid="ticket-picker-loading">Loading…</p>
      {:else if list && shown.length === 0}
        <p class="px-3 py-2 text-xs text-fg-muted" data-testid="ticket-picker-none">Nothing matches.</p>
      {:else if list?.more}
        <p class="px-3 py-2 text-xs text-fg-muted">The tracker holds more matches; type to narrow them.</p>
      {/if}
    </div>
  </div>
</Popover>
