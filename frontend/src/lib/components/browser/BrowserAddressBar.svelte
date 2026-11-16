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
  import { restorePickerFocus } from '../panes/paneComposerFocus';

  // The browser's toolbar: history, reload (stop while the page loads), the
  // address, and the pane's own controls. The address behaves as a
  // browser's: focusing it selects it all (a second click places the caret),
  // Enter navigates, and Escape puts the page's address back and leaves the
  // field for the page.

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
    /** Runs a chrome action; an omitted pageId means the active page. */
    onAct: (kind: string, pageId?: string, address?: string, index?: number) => Promise<void>;
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
  // A tab with no page to navigate yet (being created, or failed) takes no
  // address.
  let addressDisabled = $derived(!activePage || activePage.creating === true || !!activePage.error);
  let loading = $derived(activePage?.loading === true);

  /** The page's address as the field shows it: a blank page reads as empty. */
  function pageAddress(): string {
    return activePage?.url === 'about:blank' ? '' : (activePage?.url ?? '');
  }

  // The field follows the page, except while the user edits it. Another tab
  // becoming active replaces it even then: what was typed was for the tab
  // that left.
  let shownPageId = '';
  $effect(() => {
    const url = pageAddress();
    const pageId = activePage?.id ?? '';
    if (pageId !== shownPageId || !addressFocused) address = url;
    shownPageId = pageId;
  });

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
    if (!pressFocusing) input?.select();
  }

  function onMouseUp(): void {
    if (!pressFocusing || !input) return;
    pressFocusing = false;
    if (input.selectionStart === input.selectionEnd) input.select();
  }

  function onKeydown(event: KeyboardEvent): void {
    if (isImeComposingEvent(event)) return;
    if (event.key === 'Escape') {
      // The field's press: neither a find bar nor a running turn behind it
      // takes it (the app dispatcher declines a prevented event).
      event.preventDefault();
      address = pageAddress();
      onLeave();
      return;
    }
    // Until the pane knows the page it shows there is none to name, and a
    // navigation naming none opens a new page. Enter with Alt, Ctrl or Meta
    // is an app chord (alt+shift+enter maximizes the pane).
    if (isLocalEnter(event) && activePage) {
      event.preventDefault();
      const target = address;
      onLeave();
      void onAct('navigate', activePage.id, target);
    }
  }
</script>

<!-- The history buttons, the address and the pane's own controls always
     show at full size. Below a 28rem row (36rem under compact, whose
     buttons are larger) the page's secondary controls (its size,
     open file, show in folder, devtools) move into the More menu, so the
     address keeps its room. -->
<div class="@container flex h-10 shrink-0 items-center gap-0.5 border-b border-border-subtle bg-surface-0 px-1.5 compact:h-12" data-testid="browser-address-row">
  <div class="flex shrink-0 items-center gap-0.5">
    <IconButton label="Back" size="sm" disabled={!activePage?.canGoBack} onClick={() => void onAct('back')}><Icon icon={ArrowLeft} size={15} /></IconButton>
    <IconButton label="Forward" size="sm" disabled={!activePage?.canGoForward} onClick={() => void onAct('forward')}><Icon icon={ArrowRight} size={15} /></IconButton>
    {#if loading}
      <IconButton label="Stop" title="Stop loading" size="sm" testId="browser-stop" onClick={() => void onAct('stop')}><Icon icon={X} size={15} /></IconButton>
    {:else}
      <IconButton label="Reload" title="Reload{chordHintSuffix('browser.reload')}" size="sm" testId="browser-reload" disabled={!activePage || addressDisabled} onClick={() => void onAct('reload')}><Icon icon={RotateCw} size={14} /></IconButton>
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
      pressFocusing = false;
    }}
    onkeydown={onKeydown}
  />
  <div class="flex shrink-0 items-center gap-0.5">
    <div class="flex items-center gap-0.5 @max-[28rem]:hidden compact:@max-[36rem]:hidden" data-testid="browser-address-secondary">
      {#if sizeLabel}
        <span
          class="whitespace-nowrap px-1 font-mono text-[0.65rem] tabular-nums text-fg-muted"
          data-testid="browser-pane-size"
          title={SIZE_TITLE}
        >{sizeLabel}</span>
      {/if}
      <IconButton label="Open file" title="Open a file from this workspace" size="sm" testId="browser-open-file" onClick={() => onOpenFile()}><Icon icon={FileSearch} size={14} /></IconButton>
      {#if activeIsLocalFile}
        <IconButton label="Show in folder" size="sm" onClick={() => void onRevealPageFile()}><Icon icon={FolderOpen} size={14} /></IconButton>
      {/if}
      <IconButton label="Open devtools" size="sm" onClick={() => void onAct('devtools')}><Icon icon={SquareCode} size={14} /></IconButton>
    </div>
    <div class="hidden @max-[28rem]:flex compact:@max-[36rem]:flex" bind:this={moreAnchor}>
      <IconButton label="More browser actions" size="sm" testId="browser-more" ariaHaspopup="menu" ariaExpanded={moreOpen} onClick={() => (moreOpen = !moreOpen)}><Icon icon={Ellipsis} size={15} /></IconButton>
    </div>
    {#if promotePaneId}
      <PanePromoteButton paneId={promotePaneId} testId="browser-promote" size="toolbar" />
    {/if}
    <IconButton label="Close browser" size="sm" testId="browser-close" onClick={() => void onAct('hide', '')}><Icon icon={X} size={15} /></IconButton>
  </div>
</div>

<Popover anchor={moreAnchor} open={moreOpen} onClose={closeMore} placement="bottom-end" role="none">
  {#snippet children()}
    <Menu ariaLabel="More browser actions" onClose={closeMore}>
      {#if sizeLabel}
        <MenuItem label="Laid out at {sizeLabel}" title={SIZE_TITLE} disabled testId="browser-more-size" />
        <MenuDivider />
      {/if}
      <MenuItem label="Open file" onSelect={() => pick(onOpenFile)} />
      {#if activeIsLocalFile}
        <MenuItem label="Show in folder" onSelect={() => pick(() => void onRevealPageFile())} />
      {/if}
      <MenuItem label="Open devtools" onSelect={() => pick(() => void onAct('devtools'))} />
    </Menu>
  {/snippet}
</Popover>
