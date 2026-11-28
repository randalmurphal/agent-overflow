<script lang="ts" module>
  /** A value a picker offers. */
  export interface PickItem {
    readonly id: string;
    readonly name: string;
    readonly description?: string;
  }

  /** What a picker's read answered: the values, and whether the tracker
   * holds more matches than it listed. A read of several sources that
   * some answered lists theirs, and says in `failed` why each other one
   * did not, one sentence each. */
  export interface PickList {
    readonly items: readonly PickItem[];
    readonly more: boolean;
    readonly failed?: readonly string[];
  }
</script>

<script lang="ts">
  // A ticket value's pick list, at its control: the values a read answers
  // (a move list held by the view, or a form read of the tracker), and one
  // row that clears the value where the field may be empty; where it may
  // not, a field of several values keeps its last one checked. The values a
  // field holds now are checked; a pick closes the list unless the field
  // holds several.
  //
  // Search. What must hold: the list shown answers the text in the box
  // now. The opening read (empty text) says whether the tracker holds more
  // than it listed. When it does not, the list is whole and typed text
  // narrows it here, asking nothing. When it does, each pause in typing
  // asks the tracker for the text, and so does the opening answer when
  // text was typed before it landed; until the answer for the text in the
  // box lands, the last list shows narrowed here and says it is searching,
  // never that nothing matches. A search's answer or failure lands only
  // while the box holds the text it was asked for; the opening read's
  // lands while the list is open.
  import { untrack } from 'svelte';
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
    /** The control that opened the list, when a dialog hosts it: the list
     * is outside the dialog's focus trap, so Tab closes it and focus goes
     * back to this control (Popover's `claimTab`). */
    dialogTrigger?: HTMLElement;
  }

  let { anchor, open, label, load, checked, clearLabel, multi = false, onPick, onClose, dialogTrigger }: Props = $props();

  const PAUSE_MS = 250;

  let text = $state('');
  // The opening read's answer, and the last search's, with its text.
  let first = $state.raw<PickList | null>(null);
  let found = $state.raw<{ readonly text: string; readonly list: PickList } | null>(null);
  let failure = $state<string | null>(null);
  // Each opening bumps it: a read of an earlier one does not land.
  let opening = 0;
  let timer: ReturnType<typeof setTimeout> | null = null;

  const query = $derived(text.trim());
  const remote = $derived(first?.more ?? false);

  async function openRead(): Promise<void> {
    const mine = ++opening;
    try {
      const got = await load('');
      if (mine !== opening) return;
      first = got;
      // Text typed before the answer: search for it now.
      if (got.more && query) search(query);
    } catch (err) {
      if (mine === opening) failure = errString(err);
    }
  }

  async function search(words: string): Promise<void> {
    const mine = opening;
    try {
      const got = await load(words);
      if (mine === opening && words === query) found = { text: words, list: got };
    } catch (err) {
      if (mine === opening && words === query) failure = errString(err);
    }
  }

  $effect(() => {
    if (!open) return;
    text = '';
    first = null;
    found = null;
    failure = null;
    // One read per opening: a field holding several values gets a new
    // load with each pick, which must not read again.
    untrack(() => void openRead());
    return () => {
      opening += 1;
      if (timer) clearTimeout(timer);
      timer = null;
    };
  });

  function onInput(): void {
    // A failed search was for the text before.
    if (failure && first) failure = null;
    if (timer) clearTimeout(timer);
    timer = null;
    if (!remote || !query) return;
    const words = query;
    timer = setTimeout(() => {
      timer = null;
      void search(words);
    }, PAUSE_MS);
  }

  function narrowed(items: readonly PickItem[]): readonly PickItem[] {
    const words = query.toLowerCase();
    return words ? items.filter((i) => i.name.toLowerCase().includes(words)) : items;
  }

  // The answer for the text in the box, or null while it is out.
  const answer = $derived.by((): PickList | null => {
    if (!first) return null;
    if (!remote || !query) return { items: narrowed(first.items), more: remote };
    return found?.text === query ? found.list : null;
  });
  const shown = $derived(answer ? answer.items : narrowed(found?.list.items ?? first?.items ?? []));
  const searching = $derived(!!first && !answer && !failure);

  function close(reason?: PopoverCloseReason): void {
    onClose();
    restorePickerFocus(reason, { triggerEl: anchor });
  }

  function pick(item: PickItem | null): void {
    onPick(item);
    if (!multi) close();
  }
</script>

<Popover {anchor} {open} onClose={close} placement="bottom-start" ariaLabel={label} claimTab={!!dialogTrigger} restoreFocusTo={dialogTrigger}>
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
          {@const last = multi && !clearLabel && checked.size === 1 && checked.has(item.id)}
          <MenuItem label={item.name} description={item.description} checked={checked.has(item.id)} checkbox={multi} disabled={last} title={last ? 'The field must hold a value' : undefined} onSelect={() => pick(item)} testId="ticket-picker-item" />
        {/each}
      </Menu>
      {#each answer?.failed ?? [] as sentence, i (i)}
        <p class="px-3 py-2 text-xs text-error" role="alert" data-testid="ticket-picker-partial">{sentence}</p>
      {/each}
      {#if failure}
        <p class="px-3 py-2 text-xs text-error" role="alert" data-testid="ticket-picker-error">{failure}</p>
      {:else if !first}
        <p class="px-3 py-2 text-xs text-fg-muted" data-testid="ticket-picker-loading">Loading…</p>
      {:else if searching}
        <p class="px-3 py-2 text-xs text-fg-muted" data-testid="ticket-picker-searching">Searching the tracker…</p>
      {:else if answer && shown.length === 0}
        <p class="px-3 py-2 text-xs text-fg-muted" data-testid="ticket-picker-none">Nothing matches.</p>
      {:else if answer?.more}
        <p class="px-3 py-2 text-xs text-fg-muted">The tracker holds more matches; type to narrow them.</p>
      {/if}
    </div>
  </div>
</Popover>
