<script lang="ts">
  import type { PanelContext } from '../../stores/panelContext.svelte';
  import {
    attachBrowserCompanion,
    browserCompanionAct,
    browserPaneMountId,
    openBrowserTab,
    noteBrowserActionError,
    noteBrowserFindCleanupError,
    browserFindCleanupError,
  } from '../../stores/browserCompanion.svelte';
  import { addressFocusIntent, registerBrowserAddressBar, takeAddressFocus } from '../../stores/browserAddressFocus.svelte';
  import { focusBrowserPage } from '../../stores/browserPageFocus';
  import { BrowserCompanionRevealPageFile } from '../../stores/bindings';
  import { errString } from '../../utils/errors';
  import { tick, untrack } from 'svelte';
  import PaneFindBar from '../panes/PaneFindBar.svelte';
  import { companionForSource } from '../../stores/companionPanes.svelte';
  import { closePaneFind, registerPaneFindProvider } from '../../stores/paneFind.svelte';
  import { createBrowserFind } from '../../stores/browserFind.svelte';
  import BrowserAddressBar from './BrowserAddressBar.svelte';
  import BrowserFilePicker from './BrowserFilePicker.svelte';
  import { togglePanePromotion } from '../../stores/panes.svelte';
  import { isCompactLayout } from '../../stores/layoutMode.svelte';
  import { threadBackend } from '../../transport/entityIndex';
  import { HOME_BACKEND } from '../../transport/backendKey';
  import BrowserTabStrip from './BrowserTabStrip.svelte';
  import Button from '../primitives/Button.svelte';
  import { createBrowserPaneGeometry } from './browserPaneGeometry.svelte';
  import { provideBrowserPaneThread } from './browserPaneThread';

  // The pane surface is an empty HOST RECT: the platform positions a real
  // native browser view over it (spec docs/specs/embedded-browser.md §7).
  // This component's job is the chrome around it and the geometry under it —
  // it forwards no input and paints no page pixels. The native view receives
  // real OS input when focused; overlays that must paint above it hide it
  // through the airspace registry.

  let { ctx }: { ctx: PanelContext } = $props();
  provideBrowserPaneThread(() => ctx.threadId);
  let surface: HTMLDivElement | undefined = $state(undefined);
  let attachment = $state<ReturnType<typeof attachBrowserCompanion> | null>(null);

  let view = $derived(attachment?.current ?? null);
  let findCleanupError = $derived(ctx.threadId ? browserFindCleanupError(ctx.threadId) : '');
  let pages = $derived(view?.state.pages ?? []);
  let sessionName = $derived(view?.state.sessionName ?? '');
  let activePageId = $derived(view?.state.activePageId ?? '');
  let closedTabs = $derived(view?.state.closedTabs ?? 0);
  let activePage = $derived(pages.find((page) => page.id === activePageId) ?? pages[0] ?? null);
  // Boolean, not the view object: the geometry's re-report effect must fire
  // on attach, never on every page-state push.
  let attached = $derived(view !== null);

  $effect(() => {
    const threadId = ctx.threadId;
    if (!threadId) return;
    attachment = attachBrowserCompanion(threadId);
    return () => {
      attachment?.release();
      attachment = null;
    };
  });

  // Find in the page, through the engine's own find (stores/browserFind). It
  // belongs to this companion's layout pane, and exists only while the shown
  // page's engine has one: "bar" drives the shared bar, "engine" opens the
  // engine's own find UI.
  let browserPaneId = $derived(companionForSource(ctx.paneId, 'browser')?.paneId ?? null);
  let findMode = $derived(view?.state.findMode ?? '');
  $effect(() => {
    const paneId = browserPaneId;
    const threadId = ctx.threadId;
    const mode = findMode;
    if (!paneId || !threadId || (mode !== 'bar' && mode !== 'engine')) return;
    return untrack(() => {
      const find = createBrowserFind({
        threadId,
        paneId,
        mode,
        pageId: () => activePageId,
        mount: () => browserPaneMountId(threadId),
        closeBar: () => closePaneFind(paneId),
        onCleanupError: (message) => noteBrowserFindCleanupError(threadId, message),
      });
      const release = registerPaneFindProvider(paneId, find);
      return () => {
        release();
        find.dispose();
      };
    });
  });

  const geometry = createBrowserPaneGeometry({
    surface: () => surface,
    threadId: () => ctx.threadId,
    attached: () => attached,
  });

  // The page lays out at the thread's viewport. By default that is this host
  // rect, so the page fills the pane at 1:1 and nothing needs saying. The
  // label appears when the size is not the pane's: the agent pinned one, or
  // the pane is below the viewport minimum, and then carries the size and,
  // when the host rect is smaller, the percentage it is shown at.
  let viewportWidth = $derived(view?.state.viewportWidth ?? 0);
  let viewportHeight = $derived(view?.state.viewportHeight ?? 0);
  let viewportPinned = $derived(view?.state.viewportSet === true);
  let viewScale = $derived(
    viewportWidth > 0 && viewportHeight > 0 && geometry.hostWidth > 0 && geometry.hostHeight > 0
      ? Math.min(1, geometry.hostWidth / viewportWidth, geometry.hostHeight / viewportHeight)
      : 1,
  );
  let sizeLabel = $derived(
    viewportWidth > 0 && viewportHeight > 0 && (viewportPinned || viewScale < 1)
      ? `${viewportWidth} × ${viewportHeight}` + (viewScale < 1 ? ` · ${Math.round(viewScale * 100)}%` : '')
      : '',
  );

  async function act(kind: string, pageId = activePageId, nextAddress = '', index = 0): Promise<void> {
    const threadId = ctx.threadId;
    if (!threadId) return;
    await browserCompanionAct(threadId, { kind, pageId, address: nextAddress, index });
  }

  // A suspended page (its session went idle, or the app restarted) has no
  // native view. Presenting it restores it: the pane asks once each time the
  // active page is found suspended, so a refusal stays in the banner until
  // the user acts on the page. A page that crashed (suspended, with its
  // error) waits for its Reload instead, so it is not loaded again unasked.
  let restoreAsked = '';
  $effect(() => {
    const page = attached ? activePage : null;
    if (!page?.suspended || page.error) {
      restoreAsked = '';
      return;
    }
    if (page.id === restoreAsked) return;
    restoreAsked = page.id;
    void act('activate', page.id);
  });

  // A new tab the user opened takes the address bar's focus once it is ready
  // (stores/browserCompanion#openBrowserTab); the store withdraws the intent
  // when the user does anything else first or another tab is shown.
  let addressBar: ReturnType<typeof BrowserAddressBar> | undefined = $state(undefined);
  $effect(() => {
    const threadId = ctx.threadId;
    const page = activePage;
    if (!threadId || addressFocusIntent(threadId) === null) return;
    if (!untrack(() => takeAddressFocus(threadId, page))) return;
    // After this update renders the bar enabled for the ready page.
    void tick().then(() => addressBar?.focusAddress());
  });

  // The focus-address command (mod+l) reaches the bar through the store.
  $effect(() => {
    const threadId = ctx.threadId;
    if (!threadId) return;
    return registerBrowserAddressBar(threadId, () => addressBar?.focusAddress());
  });

  function openNewTab(): void {
    const threadId = ctx.threadId;
    if (threadId) void openBrowserTab(threadId, { focusAddress: true });
  }

  /**
   * Leaves the address for the page. The host rect takes DOM focus first so
   * the address field blurs at once and a pane with no page shown still
   * holds focus; the engine then gives the active page the keyboard once it
   * is shown, unless the user focuses or presses anywhere outside the host
   * rect first.
   */
  function leaveAddress(): void {
    surface?.focus();
    const threadId = ctx.threadId;
    if (threadId && surface && activePageId) void focusBrowserPage(threadId, activePageId, surface);
  }

  function togglePromotion(): void {
    if (browserPaneId && !isCompactLayout()) togglePanePromotion(browserPaneId);
  }

  // The file picker. A chosen file opens in the active tab, or in a new tab
  // when no tab can take an address, through the companion's navigation, so
  // the backend's file authorization decides and its refusal is the banner.
  let pickingFile = $state(false);
  let pickerBackend = $derived(ctx.threadId ? (threadBackend(ctx.threadId) ?? HOME_BACKEND) : HOME_BACKEND);

  function openPickedFile(path: string): void {
    pickingFile = false;
    const threadId = ctx.threadId;
    if (!threadId) return;
    const page = activePage;
    if (page && !page.creating && !(page.error && !page.suspended)) void act('navigate', page.id, path);
    else void openBrowserTab(threadId, { address: path });
  }

  // What the host rect says while the active tab has no page to present.
  let crashed = $derived(activePage?.suspended === true && !!activePage.error && !activePage.restoring);
  let pendingNote = $derived(
    crashed
      ? `This page stopped: ${activePage?.error}`
      : activePage?.error && !activePage.suspended
      ? `This tab could not be opened: ${activePage.error}`
      : activePage?.creating
        ? 'Opening…'
        : activePage?.restoring
          ? 'Restoring…'
          : '',
  );

  async function revealPageFile(): Promise<void> {
    const threadId = ctx.threadId;
    if (!threadId || !activePageId) return;
    noteBrowserActionError(threadId, '');
    try {
      await BrowserCompanionRevealPageFile(threadId, activePageId);
    } catch (err) {
      noteBrowserActionError(threadId, errString(err));
    }
  }
</script>

<div class="flex h-full min-h-0 flex-col bg-surface-0" data-testid="browser-pane">
  <BrowserTabStrip {sessionName} {pages} {activePageId} {closedTabs} onAct={act} onNewTab={openNewTab} onEmptyDoubleClick={togglePromotion} />

  <BrowserAddressBar
    bind:this={addressBar}
    {activePage}
    {sizeLabel}
    promotePaneId={browserPaneId}
    onAct={act}
    onRevealPageFile={revealPageFile}
    onOpenFile={() => (pickingFile = true)}
    onLeave={leaveAddress}
  />

  {#if pickingFile}
    <BrowserFilePicker
      workspace={ctx.workspace}
      rootPath={ctx.workspacePath ?? '~'}
      backend={pickerBackend}
      onChoose={openPickedFile}
      onClose={() => (pickingFile = false)}
    />
  {/if}

  {#if browserPaneId}
    <PaneFindBar paneId={browserPaneId} title={activePage?.title || 'Browser'} fallbackFocus={() => surface ?? null} />
  {/if}

  {#if view?.actionError}
    <div class="shrink-0 border-b border-error/25 bg-error/10 px-3 py-1.5 text-[0.7rem] text-error" role="alert">{view.actionError}</div>
  {/if}
  {#if findCleanupError}
    <div class="shrink-0 border-b border-error/25 bg-error/10 px-3 py-1.5 text-[0.7rem] text-error" role="alert" data-testid="browser-find-cleanup-error">Find could not clear the page: {findCleanupError}</div>
  {/if}

  <!-- The host rect. The native browser view is positioned exactly over this
       element by the platform host; what renders HERE is only visible when
       no view is presented (no engine, or the page is hidden). It takes DOM
       focus only as a stand-in for the page, so it draws no focus ring. -->
  <div
    bind:this={surface}
    class="relative min-h-0 flex-1 overflow-hidden bg-surface-0 outline-none"
    data-testid="browser-pane-host-rect"
    aria-label="Browser page"
    tabindex="-1"
  >
    {#if view?.error || attachment?.error}
      <div class="flex h-full items-center justify-center px-4 text-sm text-error">{view?.error || attachment?.error}</div>
    {:else if pendingNote}
      <div
        class="flex h-full flex-col items-center justify-center gap-3 px-4 text-sm {activePage?.error ? 'text-error' : 'text-fg-muted'}"
        data-testid="browser-pane-pending"
      ><span data-testid="browser-pane-pending-note">{pendingNote}</span>{#if crashed && activePage}<Button
            variant="secondary"
            size="sm"
            testId="browser-pane-crash-reload"
            onclick={() => void act('reload', activePage.id)}
          >Reload</Button>{/if}</div>
    {/if}
  </div>
</div>
