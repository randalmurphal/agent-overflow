<script lang="ts">
  import type { PanelContext } from '../../stores/panelContext.svelte';
  import {
    attachBrowserCompanion,
    browserCompanionAct,
    browserPaneMountId,
    noteBrowserActionError,
    noteBrowserFindCleanupError,
    browserFindCleanupError,
  } from '../../stores/browserCompanion.svelte';
  import { BrowserCompanionRevealPageFile } from '../../stores/bindings';
  import { errString } from '../../utils/errors';
  import { untrack } from 'svelte';
  import PaneFindBar from '../panes/PaneFindBar.svelte';
  import { companionForSource } from '../../stores/companionPanes.svelte';
  import { closePaneFind, registerPaneFindProvider } from '../../stores/paneFind.svelte';
  import { createBrowserFind } from '../../stores/browserFind.svelte';
  import BrowserAddressBar from './BrowserAddressBar.svelte';
  import BrowserTabStrip from './BrowserTabStrip.svelte';
  import { createBrowserPaneGeometry } from './browserPaneGeometry.svelte';

  // The pane surface is an empty HOST RECT: the platform positions a real
  // native browser view over it (spec docs/specs/embedded-browser.md §7).
  // This component's job is the chrome around it and the geometry under it —
  // it forwards no input and paints no page pixels. The native view receives
  // real OS input when focused; overlays that must paint above it hide it
  // through the airspace registry.

  let { ctx }: { ctx: PanelContext } = $props();
  let surface: HTMLDivElement | undefined = $state(undefined);
  let attachment = $state<ReturnType<typeof attachBrowserCompanion> | null>(null);

  let view = $derived(attachment?.current ?? null);
  let findCleanupError = $derived(ctx.threadId ? browserFindCleanupError(ctx.threadId) : '');
  let pages = $derived(view?.state.pages ?? []);
  let sessionName = $derived(view?.state.sessionName ?? '');
  let activePageId = $derived(view?.state.activePageId ?? '');
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
  // the user acts on the page.
  let restoreAsked = '';
  $effect(() => {
    const page = attached ? activePage : null;
    if (!page?.suspended) {
      restoreAsked = '';
      return;
    }
    if (page.id === restoreAsked) return;
    restoreAsked = page.id;
    void act('activate', page.id);
  });

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
  <BrowserTabStrip {sessionName} {pages} {activePageId} onAct={act} />

  <BrowserAddressBar {activePage} {sizeLabel} onAct={act} onRevealPageFile={revealPageFile} />

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
       no view is presented (no engine, or the page is hidden). -->
  <div
    bind:this={surface}
    class="relative min-h-0 flex-1 overflow-hidden bg-surface-0"
    data-testid="browser-pane-host-rect"
    aria-label="Browser page"
    tabindex="-1"
  >
    {#if view?.error || attachment?.error}
      <div class="flex h-full items-center justify-center px-4 text-sm text-error">{view?.error || attachment?.error}</div>
    {/if}
  </div>
</div>
