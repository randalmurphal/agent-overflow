<script lang="ts">
  import type { BrowserPageInfo } from '../../stores/bindings';
  import { chordHintForCommand } from '../../stores/keybindings.svelte';
  import { addToast } from '../../stores/toast.svelte';
  import { copyToClipboard } from '../../utils/clipboard';
  import { handleExternalURL, openableExternalURL } from '../../utils/externalLinks';
  import ContextMenu from '../primitives/ContextMenu.svelte';
  import MenuDivider from '../primitives/MenuDivider.svelte';
  import MenuItem from '../primitives/MenuItem.svelte';

  // A tab's right-click menu. The shared ContextMenu registers as an
  // airspace surface, so the native page yields while it is open. Every
  // action on tabs is the backend's (internal/browser/companion_tabs.go);
  // copying the address and opening it in the system browser are this
  // client's.

  let {
    x,
    y,
    page,
    index,
    tabCount,
    closedTabs,
    onAct,
    onDismiss,
  }: {
    x: number;
    y: number;
    page: BrowserPageInfo;
    /** The tab's position in the strip. */
    index: number;
    tabCount: number;
    /** How many closed tabs the thread can reopen. */
    closedTabs: number;
    onAct: (kind: string, pageId?: string) => Promise<void>;
    onDismiss: () => void;
  } = $props();

  // A tab still being created, or one that could not be, has no page to
  // reload or copy. A crashed tab (suspended, with its error) reloads.
  let hasPage = $derived(!page.creating && !(page.error && !page.suspended));
  let address = $derived(hasPage && page.url && page.url !== 'about:blank' ? page.url : '');
  let external = $derived(address ? openableExternalURL(address) : null);
  let externalTitle = $derived(
    address.toLowerCase().startsWith('file:')
      ? 'A file opens from the address row with Show in folder'
      : undefined,
  );

  // The action starts, reading this tab's props, before the dismissal
  // unmounts the menu.
  function run(action: () => void): void {
    action();
    onDismiss();
  }

  async function copyAddress(): Promise<void> {
    const ok = await copyToClipboard(address);
    addToast(ok ? 'info' : 'error', ok ? 'Copied the address.' : 'Copy failed.');
  }
</script>

<ContextMenu {x} {y} ariaLabel="Tab actions" {onDismiss} minWidthClass="min-w-[184px]">
  <MenuItem label="Reload" disabled={!hasPage} testId="browser-tab-menu-reload" onSelect={() => run(() => void onAct('reload', page.id))} />
  <MenuItem label="Duplicate" disabled={!hasPage} testId="browser-tab-menu-duplicate" onSelect={() => run(() => void onAct('duplicate', page.id))} />
  <MenuItem label="Copy address" disabled={!address} testId="browser-tab-menu-copy" onSelect={() => run(() => void copyAddress())} />
  <MenuItem
    label="Open in system browser"
    disabled={!external}
    title={externalTitle}
    testId="browser-tab-menu-external"
    onSelect={() => run(() => void (external && handleExternalURL(external)))}
  />
  <MenuDivider />
  <MenuItem label="Close" kbd={chordHintForCommand('pane.close') ?? undefined} testId="browser-tab-menu-close" onSelect={() => run(() => void onAct('close', page.id))} />
  <MenuItem label="Close other tabs" disabled={tabCount <= 1} testId="browser-tab-menu-close-others" onSelect={() => run(() => void onAct('close-others', page.id))} />
  <MenuItem label="Close tabs to the right" disabled={index >= tabCount - 1} testId="browser-tab-menu-close-right" onSelect={() => run(() => void onAct('close-right', page.id))} />
  <MenuDivider />
  <MenuItem
    label="Reopen closed tab"
    kbd={chordHintForCommand('browser.reopenTab') ?? undefined}
    disabled={closedTabs === 0}
    testId="browser-tab-menu-reopen"
    onSelect={() => run(() => void onAct('reopen'))}
  />
</ContextMenu>
