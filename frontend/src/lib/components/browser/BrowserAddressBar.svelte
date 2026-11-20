<script lang="ts">
  import ArrowLeft from '@lucide/svelte/icons/arrow-left';
  import ArrowRight from '@lucide/svelte/icons/arrow-right';
  import Ellipsis from '@lucide/svelte/icons/ellipsis';
  import FileSearch from '@lucide/svelte/icons/file-search';
  import FolderOpen from '@lucide/svelte/icons/folder-open';
  import RotateCw from '@lucide/svelte/icons/rotate-cw';
  import SquareCode from '@lucide/svelte/icons/square-code';
  import X from '@lucide/svelte/icons/x';
  import type { BrowserPageInfo } from '../../stores/bindings';
  import { browserAction, pageIdOf, type BrowserAction, type NamedTabKind } from '../../stores/browserActionWire';
  import { tabTakesAddress } from '../../stores/browserActions';
  import { chordHintSuffix } from '../../stores/keybindings.svelte';
  import { isImeComposingEvent } from '../../utils/imeComposition';
  import { isLocalEnter } from '../../utils/localEnter';
  import type { PopoverCloseReason } from '../../utils/popoverOwnership';
  import Icon from '../primitives/Icon.svelte';
  import IconButton from '../primitives/IconButton.svelte';
  import Menu from '../primitives/Menu.svelte';
  import MenuDivider from '../primitives/MenuDivider.svelte';
  import MenuItem from '../primitives/MenuItem.svelte';
  import Popover from '../primitives/Popover.svelte';
  import PanePromoteButton from '../panes/PanePromoteButton.svelte';
  import BrowserDownloadsControl from './BrowserDownloadsControl.svelte';
  import BrowserJarControl from './BrowserJarControl.svelte';
  import BrowserAddressSuggestions from './BrowserAddressSuggestions.svelte';
  import type { AddressSuggestion } from './browserAddressMatch';
  import { restorePickerFocus } from '../panes/paneComposerFocus';
  import { intendFocus, type FocusIntent } from '../../utils/focusIntent';
  import { onDestroy, untrack } from 'svelte';
  import { addressOwner } from './browserAddressOwner';
  import { browserPaneThread } from './browserPaneThread';
  import { browserPaneMountId } from '../../stores/browserCompanion.svelte';
  import { browserSelectionsSent } from '../../stores/browserActionWire';

  // The browser's toolbar: history, reload (stop while the page loads), the
  // address, and the pane's own controls. The address behaves as a
  // browser's: focusing it selects it all (a second click places the caret),
  // typing offers suggestions (BrowserAddressSuggestions), Enter navigates,
  // and Escape closes the suggestions, or with none open puts the page's
  // address back and leaves the field for the page.

  let {
    activePage,
    sizeLabel,
    promotePaneId,
    onAct,
    onRevealPageFile,
    onOpenFile,
    onLeave,
  }: {
    activePage: BrowserPageInfo | null;
    /** The page's layout size when it is not the pane's; empty hides it. */
    sizeLabel: string;
    /** The browser companion's layout pane id, for maximize and restore. */
    promotePaneId: string | null;
    /** Runs a chrome action. */
    onAct: (action: BrowserAction) => Promise<void>;
    onRevealPageFile: () => Promise<void>;
    /** Opens the file picker. */
    onOpenFile: () => void;
    /** Moves focus out of the address to the page area (Escape, Enter). */
    onLeave: () => void;
  } = $props();

  const SIZE_TITLE = 'The page is laid out at this size and shown scaled to fit the pane';

  // The More menu: the secondary controls once the row is too narrow for
  // them (the markup's container query decides which shows).
  let moreOpen = $state(false);
  let moreAnchor: HTMLElement | undefined = $state(undefined);
  let jarControl: ReturnType<typeof BrowserJarControl> | undefined = $state(undefined);
  let jarLabel = $derived.by(() => jarControl?.currentLabel() ?? null);

  function closeMore(reason?: PopoverCloseReason): void {
    moreOpen = false;
    restorePickerFocus(reason, { triggerEl: moreAnchor?.querySelector<HTMLElement>('button') ?? undefined });
  }

  // The chosen row is removed with the menu, so focus goes back to More
  // before the action runs: a dialog the action opens returns focus there.
  function pick(action: () => void): void {
    moreOpen = false;
    moreAnchor?.querySelector<HTMLElement>('button')?.focus();
    action();
  }

  let address = $state('');
  let addressFocused = $state(false);
  let input: HTMLInputElement | undefined = $state(undefined);
  let activeIsLocalFile = $derived((activePage?.url ?? '').toLowerCase().startsWith('file:'));
  // A tab with no page to navigate yet takes no address (tabTakesAddress).
  let addressDisabled = $derived(!tabTakesAddress(activePage));
  let loading = $derived(activePage?.loading === true);

  /** The page's address as the field shows it: a blank page reads as empty. */
  function pageAddress(): string {
    return activePage?.url === 'about:blank' ? '' : (activePage?.url ?? '');
  }

  // A draft belongs to the tab that owns what the user types
  // (addressOwner: the tab a suggestion switch the user chose is on its way
  // to, until it is shown or another selection supersedes it; otherwise the
  // tab shown). The field shows its tab's draft while there is one, and the
  // active tab's address otherwise. A page push never replaces a draft (its
  // page's address changing, the chosen switch shown); the owner becoming
  // another tab drops it, as a tab switch replaces an edit with the new
  // tab's address. Enter submits a draft to its own tab, and committing it
  // (Enter, a suggestion) or abandoning it (Escape, leaving the field)
  // gives the field back to the page. This effect is the one place the
  // field takes the page's address.
  let draft: { pageId: string; text: string } | null = $state(null);
  const thread = browserPaneThread();
  let mount = $derived.by(() => {
    const threadId = thread();
    return threadId ? browserPaneMountId(threadId) : null;
  });
  // The suggestion switch on its way: its tab, whether the state has shown
  // that tab since, and the mount's selection count once it was sent, past
  // which a later selection supersedes it.
  let switching = $state<{ pageId: string; observed: boolean; sentAt: number } | null>(null);
  $effect(() => {
    if (switching && !switching.observed && activePage?.id === switching.pageId) switching.observed = true;
  });
  let fieldPageId = $derived(addressOwner(
    activePage?.id ?? null,
    switching && {
      pageId: switching.pageId,
      observed: switching.observed,
      superseded: mount !== null && browserSelectionsSent(mount) > switching.sentAt,
    },
  ));
  $effect(() => {
    const url = pageAddress();
    const pageId = fieldPageId;
    if (draft && draft.pageId === pageId) return;
    untrack(() => {
      draft = null;
      showAddress(url);
    });
  });

  /**
   * Shows `url` in the field. A focused field with all its text selected
   * keeps the new text selected, so the user's typing still replaces it.
   */
  function showAddress(url: string): void {
    const field = input;
    const selectedAll = !!field && document.activeElement === field && field.selectionStart === 0 && field.selectionEnd === field.value.length;
    address = url;
    if (field && selectedAll && field.value !== url) {
      field.value = url;
      field.select();
    }
  }

  /** Focuses the address with its text selected (a new tab, mod+l). */
  export function focusAddress(): void {
    if (!input || input.disabled) return;
    input.focus();
    input.select();
  }

  // Select-all on focus. A pointer press that focuses the field would place
  // the caret after the focus handler selected, so a press focusing it
  // selects on its release instead, unless it dragged out a selection of its
  // own. A press on the focused field places the caret as usual.
  let pressFocusing = false;

  function onPointerDown(): void {
    pressFocusing = document.activeElement !== input;
  }

  function onFocus(): void {
    addressFocused = true;
    suggestions?.focused();
    if (!pressFocusing) input?.select();
  }

  function onMouseUp(): void {
    if (!pressFocusing || !input) return;
    pressFocusing = false;
    if (input.selectionStart === input.selectionEnd) input.select();
  }

  function onKeydown(event: KeyboardEvent): void {
    if (isImeComposingEvent(event)) return;
    if (suggestions?.keydown(event)) return;
    if (event.key === 'Escape') {
      // The field's press: neither a find bar nor a running turn behind it
      // takes it (the app dispatcher declines a prevented event).
      event.preventDefault();
      draft = null;
      onLeave();
      return;
    }
    // Until the pane knows the page it shows there is none to name, and a
    // navigation naming none opens a new page. Enter with Alt, Ctrl or Meta
    // is an app chord (alt+shift+enter maximizes the pane). A draft goes to
    // its own tab; an unedited field navigates the active tab to its text.
    const pageId = pageIdOf(draft?.pageId ?? activePage?.id);
    if (isLocalEnter(event) && pageId) {
      event.preventDefault();
      const target = draft?.text ?? address;
      draft = null;
      onLeave();
      void onAct(browserAction.navigate(pageId, target));
    }
  }

  // A suggestion is taken as Enter takes typed text. A tab is switched to
  // first, so the field leaves for that tab's page once the switch is
  // answered. That leave is an intent taken when the tab is chosen: the
  // user's next edit of the field, or a focus or press anywhere else, ends
  // it, so a slow switch moves no focus away from what the user did
  // meanwhile, and an edit made meanwhile is a draft its answer leaves alone.
  let suggestions: ReturnType<typeof BrowserAddressSuggestions> | undefined = $state(undefined);
  let leaveAfterSwitch: FocusIntent | null = null;
  onDestroy(() => leaveAfterSwitch?.end());
  function takeSuggestion(suggestion: AddressSuggestion): void {
    const pageId = pageIdOf(fieldPageId);
    if (!pageId || !input) return;
    draft = null;
    const suggested = pageIdOf(suggestion.pageId);
    if (suggested) {
      const field = input;
      const intent = intendFocus({ holdsFocus: (node) => field.contains(node), holdsPress: () => false });
      leaveAfterSwitch = intent;
      const sending = onAct(browserAction.select(suggested));
      // The selection was counted as it was sent, in that call.
      switching = { pageId: suggested, observed: false, sentAt: mount ? browserSelectionsSent(mount) : 0 };
      // The state's own proxy, which later reads of `switching` return.
      const pending = switching;
      void sending.then(() => {
        // Answered: the switch was shown or superseded, or did not happen
        // and the pane shows why; either way the tab shown owns the field.
        if (switching === pending) switching = null;
        intent.land(() => {
          if (document.activeElement === field) onLeave();
        });
      });
      return;
    }
    onLeave();
    void onAct(browserAction.navigate(pageId, suggestion.address));
  }

  // A toolbar command on the page the pane shows; nothing before it has one.
  function onActivePage(kind: NamedTabKind): void {
    const pageId = pageIdOf(activePage?.id);
    if (pageId) void onAct(browserAction.onTab(kind, pageId));
  }
</script>

<!-- The history buttons, the address, the downloads button (once the
     thread has a download) and the pane's own controls always show at
     full size. Below a 32rem row (36rem under compact, whose
     buttons are larger) the page's secondary controls (its size, cookie
     jar, open file, show in folder, devtools) move into the More menu, so
     the address keeps its room. -->
<div class="@container flex h-10 shrink-0 items-center gap-0.5 border-b border-border-subtle bg-surface-0 px-1.5 compact:h-12" data-testid="browser-address-row">
  <div class="flex shrink-0 items-center gap-0.5">
    <IconButton label="Back" size="sm" disabled={!activePage?.canGoBack} onClick={() => onActivePage('back')}><Icon icon={ArrowLeft} size={15} /></IconButton>
    <IconButton label="Forward" size="sm" disabled={!activePage?.canGoForward} onClick={() => onActivePage('forward')}><Icon icon={ArrowRight} size={15} /></IconButton>
    {#if loading}
      <IconButton label="Stop" title="Stop loading" size="sm" testId="browser-stop" onClick={() => onActivePage('stop')}><Icon icon={X} size={15} /></IconButton>
    {:else}
      <IconButton label="Reload" title="Reload{chordHintSuffix('browser.reload')}" size="sm" testId="browser-reload" disabled={addressDisabled} onClick={() => onActivePage('reload')}><Icon icon={RotateCw} size={14} /></IconButton>
    {/if}
  </div>
  <input
    class="mx-1 h-7 min-w-24 flex-1 rounded-full border border-transparent bg-surface-1 px-3 text-[0.75rem] text-fg outline-none transition-colors placeholder:text-fg-hint hover:bg-surface-2 focus:border-accent/60 focus:bg-surface-0 disabled:opacity-60 compact:h-9"
    aria-label="Address"
    placeholder="Search or enter an address"
    title={addressFocused ? undefined : `${pageAddress()}${chordHintSuffix('browser.focusAddress')}`}
    spellcheck="false"
    autocomplete="off"
    bind:this={input}
    bind:value={address}
    disabled={addressDisabled}
    onpointerdown={onPointerDown}
    onmouseup={onMouseUp}
    onfocus={onFocus}
    onblur={() => {
      addressFocused = false;
      draft = null;
      pressFocusing = false;
      suggestions?.close();
    }}
    oninput={(event) => {
      const pageId = fieldPageId;
      draft = pageId ? { pageId, text: event.currentTarget.value } : null;
      leaveAfterSwitch?.end();
      suggestions?.input(event.currentTarget.value);
    }}
    onkeydown={onKeydown}
    {...suggestions?.inputAria()}
  />
  <div class="flex shrink-0 items-center gap-0.5">
    <div class="flex items-center gap-0.5 @max-[32rem]:hidden compact:@max-[36rem]:hidden" data-testid="browser-address-secondary">
      {#if sizeLabel}
        <span
          class="whitespace-nowrap px-1 font-mono text-[0.65rem] tabular-nums text-fg-muted"
          data-testid="browser-pane-size"
          title={SIZE_TITLE}
        >{sizeLabel}</span>
      {/if}
      <BrowserJarControl bind:this={jarControl} />
      <IconButton label="Open file" title="Open a file from this workspace" size="sm" testId="browser-open-file" onClick={() => onOpenFile()}><Icon icon={FileSearch} size={14} /></IconButton>
      {#if activeIsLocalFile}
        <IconButton label="Show in folder" size="sm" onClick={() => void onRevealPageFile()}><Icon icon={FolderOpen} size={14} /></IconButton>
      {/if}
      <IconButton label="Open devtools" size="sm" onClick={() => onActivePage('devtools')}><Icon icon={SquareCode} size={14} /></IconButton>
    </div>
    <BrowserDownloadsControl />
    <div class="hidden @max-[32rem]:flex compact:@max-[36rem]:flex" bind:this={moreAnchor}>
      <IconButton label="More browser actions" size="sm" testId="browser-more" ariaHaspopup="menu" ariaExpanded={moreOpen} onClick={() => (moreOpen = !moreOpen)}><Icon icon={Ellipsis} size={15} /></IconButton>
    </div>
    {#if promotePaneId}
      <PanePromoteButton paneId={promotePaneId} testId="browser-promote" size="toolbar" />
    {/if}
    <IconButton label="Close browser" size="sm" testId="browser-close" onClick={() => void onAct(browserAction.hide())}><Icon icon={X} size={15} /></IconButton>
  </div>
</div>

<BrowserAddressSuggestions bind:this={suggestions} anchor={input} onPick={takeSuggestion} />

<Popover anchor={moreAnchor} open={moreOpen} onClose={closeMore} placement="bottom-end" role="none">
  {#snippet children()}
    <Menu ariaLabel="More browser actions" onClose={closeMore}>
      {#if sizeLabel}
        <MenuItem label="Laid out at {sizeLabel}" title={SIZE_TITLE} disabled testId="browser-more-size" />
        <MenuDivider />
      {/if}
      {#if jarLabel}
        <MenuItem label="Cookie jar" description={jarLabel} testId="browser-more-jar" onSelect={() => pick(() => jarControl?.openMenu(moreAnchor))} />
      {/if}
      <MenuItem label="Open file" onSelect={() => pick(onOpenFile)} />
      {#if activeIsLocalFile}
        <MenuItem label="Show in folder" onSelect={() => pick(() => void onRevealPageFile())} />
      {/if}
      <MenuItem label="Open devtools" onSelect={() => pick(() => onActivePage('devtools'))} />
    </Menu>
  {/snippet}
</Popover>
