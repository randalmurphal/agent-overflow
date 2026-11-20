<script lang="ts">
  import AppWindow from '@lucide/svelte/icons/app-window';
  import Clock from '@lucide/svelte/icons/clock';
  import Server from '@lucide/svelte/icons/server';
  import { untrack } from 'svelte';
  import { threadMachine } from '../../stores/attachedBackends.svelte';
  import type { BrowserRecentAddress } from '../../stores/bindings';
  import { readRecentAddresses } from '../../stores/browserActionWire';
  import { browserCompanionState, browserPaneAttachment, type BrowserPaneAttachment } from '../../stores/browserCompanion.svelte';
  import { loadDevServers, machineDevServers } from '../../stores/devServers.svelte';
  import { isLocalEnter } from '../../utils/localEnter';
  import { randomId } from '../../utils/randomId';
  import Icon from '../primitives/Icon.svelte';
  import Popover from '../primitives/Popover.svelte';
  import { addressSuggestions, threadDevServers, type AddressSuggestion } from './browserAddressMatch';
  import { browserPaneThread } from './browserPaneThread';

  // The address field's suggestion list (browserAddressMatch.ts). The
  // field forwards its focus, input and keys here. The list opens on typing
  // and paints over the page, so it is a Popover, which registers in the
  // airspace registry and makes the native view yield while it is open. It
  // registers as a layer, not an overlay: the field keeps the keyboard, so
  // the browser pane's commands (Mod+R, Ctrl+Tab) stay live under it.
  // Arrows move the highlight, Enter takes it, Escape closes the list (the
  // field's own Escape then reverts on the next press), and the typed text is
  // never changed by the list.

  let {
    anchor,
    onPick,
  }: {
    /** The address field the list hangs under. */
    anchor: HTMLElement | undefined;
    onPick: (suggestion: AddressSuggestion) => void;
  } = $props();

  const thread = browserPaneThread();
  let threadId = $derived(thread());
  // The pane attachment the list reads from: null while the pane has none
  // or this window may not host the thread's browser, and then no recent
  // address is read or shown.
  let pane = $derived(threadId ? browserPaneAttachment(threadId) : null);
  let browser = $derived(threadId ? browserCompanionState(threadId) : null);
  let activePageId = $derived(browser?.activePageId ?? '');
  let machine = $derived(threadId ? machineDevServers(threadMachine(threadId, null)) : null);

  let query = $state('');
  let open = $state(false);
  // The highlighted row by its key, so a list that changes under it (a tab
  // push, a dev-server scan) never moves Enter to another row; a key no
  // longer listed is no highlight, which is the typed text.
  let highlightKey = $state<string | null>(null);

  // The recent addresses are read when the field takes focus, not carried by
  // every state push: only a focused field reads them. They belong to the
  // owner below and are never shown for another, and are read only from
  // the pane's attachment (readRecentAddresses checks it again at the
  // answer).
  let recent = $state.raw<readonly BrowserRecentAddress[]>([]);
  let recentError = $state('');
  let readSeq = 0;

  async function readRecent(from: BrowserPaneAttachment): Promise<void> {
    const seq = ++readSeq;
    const answer = await readRecentAddresses(from);
    if (seq !== readSeq) return;
    switch (answer.kind) {
      case 'answered':
        recent = answer.value;
        recentError = '';
        return;
      case 'refused':
        recent = [];
        recentError = `Recent addresses could not be read: ${answer.error}`;
        return;
      case 'pane-gone':
        recent = [];
        recentError = '';
        return;
      default:
        return unhandledRecent(answer);
    }
  }

  function unhandledRecent(answer: never): never {
    throw new Error(`Unhandled recent address answer: ${JSON.stringify(answer)}`);
  }

  let items = $derived(
    open && threadId
      ? addressSuggestions(query, {
          tabs: browser?.pages ?? [],
          activePageId,
          devServers: threadDevServers(threadId, machine?.list?.servers ?? []),
          recent: pane ? recent : [],
        })
      : [],
  );
  let highlight = $derived(highlightKey === null ? -1 : items.findIndex((item) => item.key === highlightKey));
  let errors = $derived([recentError, machine?.loadError ?? ''].filter((text) => text !== ''));
  let shown = $derived(open && (items.length > 0 || errors.length > 0));

  // What the field and the list are for: the thread, its pane attachment,
  // its tab, and the Manager's forget generation of recent addresses
  // (internal/browser/recent.go), which every forget advances, so a round
  // trip of jar names cannot hide one. An attachment lost to an owner that
  // moved or became contested drops the list. Another tab or thread replaces the field's text, so the
  // list described what was typed for the one that left; and a forgotten
  // list is not the Manager's any more. Any change drops it and any read in
  // flight, and reads again while the field has focus. The generation is
  // Manager-wide: a forget on another thread costs one re-read here at the
  // next push.
  let owner = $derived(`${threadId ?? ''}\n${pane?.mount ?? ''}\n${activePageId}\n${browser?.recentGen ?? ''}`);
  let ownedBy = '';
  $effect(() => {
    const key = owner;
    if (key === ownedBy) return;
    ownedBy = key;
    untrack(() => {
      close();
      recent = [];
      recentError = '';
      readSeq++;
      if (pane && fieldFocused()) void readRecent(pane);
    });
  });

  function fieldFocused(): boolean {
    const active = document.activeElement;
    return !!anchor && !!active && (anchor === active || anchor.contains(active));
  }

  const listId = `browser-address-suggestions-${randomId()}`;
  const optionId = (index: number) => `${listId}-${index}`;

  /** The field took focus: read what the list draws from. */
  export function focused(): void {
    close();
    const id = threadId;
    if (!id) return;
    if (pane) void readRecent(pane);
    // A dev server the thread started since the last read appears only on a
    // new scan; a passive load, so a session without the scope asks nothing.
    void loadDevServers(threadMachine(id, null));
  }

  /** The field's text changed by typing. */
  export function input(text: string): void {
    query = text;
    open = text.trim() !== '';
    highlightKey = null;
  }

  export function close(): void {
    open = false;
    highlightKey = null;
  }

  /** The field's attributes naming the list and its highlighted row. */
  export function inputAria(): Record<string, string | undefined> {
    return {
      'aria-autocomplete': 'list',
      'aria-controls': shown && items.length > 0 ? listId : undefined,
      'aria-activedescendant': shown && highlight >= 0 ? optionId(highlight) : undefined,
    };
  }

  function pick(suggestion: AddressSuggestion): void {
    close();
    onPick(suggestion);
  }

  /**
   * A key pressed in the field; true when the list took it. The field
   * checks IME composition before asking.
   */
  export function keydown(event: KeyboardEvent): boolean {
    if (!shown) return false;
    const count = items.length;
    if (event.key === 'Escape') {
      // Closing is this press; the field's revert waits for the next one.
      event.preventDefault();
      event.stopPropagation();
      close();
      return true;
    }
    if ((event.key === 'ArrowDown' || event.key === 'ArrowUp') && count > 0) {
      event.preventDefault();
      // -1 is the typed text itself, between the last row and the first.
      const step = event.key === 'ArrowDown' ? 1 : -1;
      const next = ((highlight + 1 + step + count + 1) % (count + 1)) - 1;
      highlightKey = next >= 0 ? items[next].key : null;
      return true;
    }
    if (isLocalEnter(event) && highlight >= 0 && highlight < count) {
      event.preventDefault();
      pick(items[highlight]);
      return true;
    }
    return false;
  }

  const ICONS = { tab: AppWindow, 'dev-server': Server, recent: Clock } as const;
</script>

<Popover {anchor} open={shown} onClose={() => close()} placement="bottom-start" matchAnchorWidth takesKeyboard={false}>
  {#snippet children()}
    <!-- The rows keep focus in the field (mousedown), so the field's keys
         and its blur go on describing the list. -->
    <div
      class="min-w-80 overflow-hidden rounded-[var(--radius-control)] border border-border-subtle bg-surface-1 py-1 shadow-menu"
      data-testid="browser-address-suggestions"
    >
      {#if items.length > 0}
        <ul id={listId} role="listbox" aria-label="Address suggestions">
          {#each items as item, index (item.key)}
            <!-- svelte-ignore a11y_click_events_have_key_events — the address field drives the keyboard -->
            <li
              id={optionId(index)}
              role="option"
              aria-selected={highlight === index}
              data-kind={item.kind}
              data-testid="browser-address-suggestion"
              class="flex cursor-pointer select-none items-center gap-2 px-2.5 py-1 text-[0.75rem] {highlight === index ? 'bg-accent/15 text-fg' : 'text-fg-muted hover:bg-surface-2/40 hover:text-fg'} compact:py-2"
              title={item.address}
              onmousedown={(event) => event.preventDefault()}
              onmousemove={() => (highlightKey = item.key)}
              onclick={() => pick(item)}
            >
              <Icon icon={ICONS[item.kind]} size={13} class="shrink-0 text-fg-muted" />
              <!-- Two lines, so a narrow address field still shows the address. -->
              <span class="flex min-w-0 flex-1 flex-col">
                {#if item.title}<span class="truncate text-fg">{item.title}</span>{/if}
                <span class="truncate font-mono text-[0.6875rem] {item.title ? '' : 'text-fg'}">{item.address}</span>
              </span>
              {#if item.kind === 'tab'}
                <span class="shrink-0 text-[0.6875rem] text-fg-hint">Switch to tab</span>
              {/if}
            </li>
          {/each}
        </ul>
      {/if}
      {#each errors as error (error)}
        <div class="px-2.5 py-1 text-[0.6875rem] text-error" role="alert" data-testid="browser-address-suggestions-error">{error}</div>
      {/each}
    </div>
  {/snippet}
</Popover>
