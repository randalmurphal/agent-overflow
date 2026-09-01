<script lang="ts">
  import ArrowLeft from '@lucide/svelte/icons/arrow-left';
  import ArrowRight from '@lucide/svelte/icons/arrow-right';
  import FolderOpen from '@lucide/svelte/icons/folder-open';
  import Plus from '@lucide/svelte/icons/plus';
  import RefreshCw from '@lucide/svelte/icons/refresh-cw';
  import SquareCode from '@lucide/svelte/icons/square-code';
  import X from '@lucide/svelte/icons/x';
  import type { PanelContext } from '../../stores/panelContext.svelte';
  import {
    attachBrowserCompanion,
    applyBrowserCompanionState,
    reportBrowserPaneRect,
  } from '../../stores/browserCompanion.svelte';
  import {
    BrowserCompanionAction,
    BrowserCompanionDo,
    BrowserCompanionRevealPageFile,
  } from '../../stores/bindings';
  import Icon from '../primitives/Icon.svelte';
  import { errString } from '../../utils/errors';
  import { airspaceIntersects, airspaceSurfaces } from '../../utils/paneAirspace.svelte';
  import { rgbChannels, toConcreteColor } from '../../utils/cssColorProbe';
  import { getPaneLayoutItems } from '../../stores/paneLayout.svelte';
  import { getSidebarWidth, isSidebarCollapsed } from '../../stores/sidebarLayout.svelte';
  import { getAppliedTheme } from '../../theme/themeApply.svelte';
  import { isMacPlatform } from '../../utils/platform';

  // The pane surface is an empty HOST RECT: the platform positions a real
  // native browser view over it (spec docs/specs/embedded-browser.md §7).
  // This component's job is the chrome around it and the geometry under it —
  // it forwards no input and paints no page pixels. The native view receives
  // real OS input when focused; overlays that must paint above it hide it
  // through the airspace registry.

  let { ctx }: { ctx: PanelContext } = $props();
  let surface: HTMLDivElement | undefined = $state(undefined);
  let attachment = $state<ReturnType<typeof attachBrowserCompanion> | null>(null);
  let address = $state('');
  let addressFocused = $state(false);
  let error = $state('');
  let rectFrame = 0;
  let lastSentKey = '';
  let bgRaw = '';
  let bgHex = '';

  let view = $derived(attachment?.current ?? null);
  let pages = $derived(view?.state.pages ?? []);
  let sessionName = $derived(view?.state.sessionName ?? '');
  let activePageId = $derived(view?.state.activePageId ?? '');
  let activePage = $derived(pages.find((page) => page.id === activePageId) ?? pages[0] ?? null);
  let activeIsLocalFile = $derived((activePage?.url ?? '').toLowerCase().startsWith('file:'));
  // Boolean, not the view object: the re-report effect below must fire on
  // attach, never on every page-state push.
  let attached = $derived(view !== null);

  $effect(() => {
    const threadId = ctx.threadId;
    if (!threadId) return;
    attachment = attachBrowserCompanion(threadId);
    return () => {
      if (rectFrame) cancelAnimationFrame(rectFrame);
      rectFrame = 0;
      lastSentKey = '';
      attachment?.release();
      attachment = null;
    };
  });

  $effect(() => {
    const url = activePage?.url ?? '';
    if (!addressFocused) address = url;
  });

  // ─── Host-rect geometry ────────────────────────────────────────────────
  // One report per changed frame: every signal below only SCHEDULES, and the
  // flush reads geometry once. There is deliberately no standing rAF loop —
  // a per-frame callback would pin frame production at panel refresh.

  function scheduleRect(): void {
    if (rectFrame) return;
    rectFrame = requestAnimationFrame(() => {
      rectFrame = 0;
      flushRect();
    });
  }

  // A native view cannot be partially cropped by the DOM, so the report
  // carries the VISIBLE INTERSECTION of the host rect with every clipping
  // ancestor (and the viewport): the platform host crops the view to it
  // while the page keeps the full rect's size. A pane half behind the
  // sidebar shows its visible half instead of hiding or overhanging.
  function visibleClip(el: HTMLElement, rect: DOMRect): { left: number; top: number; right: number; bottom: number } {
    let left = Math.max(rect.left, 0);
    let top = Math.max(rect.top, 0);
    let right = Math.min(rect.right, window.innerWidth);
    let bottom = Math.min(rect.bottom, window.innerHeight);
    for (let node = el.parentElement; node; node = node.parentElement) {
      const style = getComputedStyle(node);
      if (style.overflowX === 'visible' && style.overflowY === 'visible') continue;
      const r = node.getBoundingClientRect();
      if (style.overflowX !== 'visible') {
        left = Math.max(left, r.left);
        right = Math.min(right, r.right);
      }
      if (style.overflowY !== 'visible') {
        top = Math.max(top, r.top);
        bottom = Math.min(bottom, r.bottom);
      }
    }
    return { left, top, right, bottom };
  }

  // The pane surface's resolved background color, memoized on the computed
  // string so the canvas round trip only runs when the theme actually
  // changes; the theme-change trigger below clears the memo.
  function paneBackground(el: HTMLElement): string {
    const raw = getComputedStyle(el).backgroundColor;
    if (raw === bgRaw) return bgHex;
    bgRaw = raw;
    const channels = rgbChannels(toConcreteColor(raw));
    bgHex =
      channels && channels.a === 1
        ? '#' + [channels.r, channels.g, channels.b].map((c) => c.toString(16).padStart(2, '0')).join('')
        : '';
    return bgHex;
  }

  const round2 = (value: number): number => Math.round(value * 100) / 100;

  function flushRect(): void {
    const el = surface;
    const threadId = ctx.threadId;
    if (!el || !threadId) return;
    const rect = el.getBoundingClientRect();
    const clip = visibleClip(el, rect);
    const clipWidth = Math.max(0, clip.right - clip.left);
    const clipHeight = Math.max(0, clip.bottom - clip.top);
    // Obscurity is judged against the VISIBLE part: an overlay above a
    // clipped-away strip does not hide the pane.
    const visible =
      rect.width >= 1 &&
      rect.height >= 1 &&
      clipWidth >= 1 &&
      clipHeight >= 1 &&
      !airspaceIntersects({ left: clip.left, top: clip.top, right: clip.right, bottom: clip.bottom });
    const report = {
      x: round2(rect.left),
      y: round2(rect.top),
      width: round2(rect.width),
      height: round2(rect.height),
      clipX: round2(clip.left),
      clipY: round2(clip.top),
      clipWidth: round2(clipWidth),
      clipHeight: round2(clipHeight),
      viewportWidth: window.innerWidth,
      viewportHeight: window.innerHeight,
      visible,
      background: paneBackground(el),
    };
    const key =
      `${report.x},${report.y},${report.width},${report.height},` +
      `${report.clipX},${report.clipY},${report.clipWidth},${report.clipHeight},` +
      `${report.viewportWidth},${report.viewportHeight},${report.visible},${report.background}`;
    if (key === lastSentKey) return;
    lastSentKey = key;
    reportBrowserPaneRect(threadId, report);
  }

  $effect(() => {
    const el = surface;
    if (!el || !ctx.threadId) return;
    const observer = new ResizeObserver(() => scheduleRect());
    observer.observe(el);
    const onLayoutShift = (): void => scheduleRect();
    window.addEventListener('resize', onLayoutShift);
    // Capture-phase: a scroll anywhere in an ancestor chain moves the rect
    // without resizing it (the pane strip scrolling behind the sidebar).
    document.addEventListener('scroll', onLayoutShift, true);
    scheduleRect();
    return () => {
      observer.disconnect();
      window.removeEventListener('resize', onLayoutShift);
      document.removeEventListener('scroll', onLayoutShift, true);
    };
  });

  // Overlay open/close re-evaluates obscurity; the mount acquiring its pane
  // id re-sends the rect a too-early report dropped.
  $effect(() => {
    airspaceSurfaces();
    if (attached) lastSentKey = '';
    scheduleRect();
  });

  // Layout-driven POSITION changes: a divider drag on another pane slides
  // this pane sideways without resizing it, which no ResizeObserver, window
  // resize or scroll event reports (live incident 2026-08-31: the native
  // view stayed planted until the next scroll). Pane widths and the sidebar
  // are the app's own movers, so reading them here re-measures on every
  // frame of a drag. Over-firing is fine — flushRect dedupes.
  $effect(() => {
    for (const item of getPaneLayoutItems()) void item.widthPx;
    void getSidebarWidth();
    void isSidebarCollapsed();
    scheduleRect();
  });

  // A theme or mode change recolors the pane surface without moving it.
  $effect(() => {
    void getAppliedTheme();
    bgRaw = '';
    scheduleRect();
  });

  async function act(kind: string, pageId = activePageId, nextAddress = '', index = 0): Promise<void> {
    const threadId = ctx.threadId;
    if (!threadId) return;
    error = '';
    try {
      const state = await BrowserCompanionDo(threadId, new BrowserCompanionAction({
        kind,
        pageId,
        address: nextAddress,
        index,
      }));
      applyBrowserCompanionState(state);
    } catch (err) {
      error = errString(err);
    }
  }

  async function revealPageFile(): Promise<void> {
    const threadId = ctx.threadId;
    if (!threadId || !activePageId) return;
    error = '';
    try {
      await BrowserCompanionRevealPageFile(threadId, activePageId);
    } catch (err) {
      error = errString(err);
    }
  }

  // ─── Tab drag-reorder ──────────────────────────────────────────────────
  // Same HTML5 DnD protocol the pane host uses for pane reordering. The
  // drop slot is a thin insertion bar between tabs; the drop commits a
  // "move" action and the authoritative order comes back with the state.

  const TAB_DRAG_MIME = 'application/x-ao-browser-tab';
  let dragPageId = $state('');
  let dropSlot = $state(-1);

  function tabDragStart(event: DragEvent, pageId: string): void {
    if (!event.dataTransfer) return;
    event.dataTransfer.setData(TAB_DRAG_MIME, pageId);
    event.dataTransfer.effectAllowed = 'move';
    dragPageId = pageId;
  }

  function tabDragOver(event: DragEvent, index: number): void {
    if (!dragPageId || !event.dataTransfer?.types.includes(TAB_DRAG_MIME)) return;
    event.preventDefault();
    event.dataTransfer.dropEffect = 'move';
    const tab = event.currentTarget as HTMLElement;
    const rect = tab.getBoundingClientRect();
    dropSlot = event.clientX < rect.left + rect.width / 2 ? index : index + 1;
  }

  function stripDragOver(event: DragEvent): void {
    if (!dragPageId || !event.dataTransfer?.types.includes(TAB_DRAG_MIME)) return;
    // Only the strip's own empty tail; a tab under the pointer already set
    // its slot and stopped the event from bubbling this far unhandled.
    if (event.target !== event.currentTarget) return;
    event.preventDefault();
    event.dataTransfer.dropEffect = 'move';
    dropSlot = pages.length;
  }

  function tabDrop(event: DragEvent): void {
    const pageId = event.dataTransfer?.getData(TAB_DRAG_MIME) ?? '';
    const slot = dropSlot;
    dragPageId = '';
    dropSlot = -1;
    if (!pageId || slot < 0) return;
    event.preventDefault();
    const from = pages.findIndex((page) => page.id === pageId);
    if (from < 0) return;
    // The backend's move index addresses the list WITHOUT the moved tab.
    const to = from < slot ? slot - 1 : slot;
    if (to === from) return;
    void act('move', pageId, '', to);
  }

  function tabDragEnd(): void {
    dragPageId = '';
    dropSlot = -1;
  }

  function closeTabShortcut(event: KeyboardEvent): void {
    if (event.isComposing || event.key.toLowerCase() !== 'w' || event.altKey || event.shiftKey) return;
    const modifier = isMacPlatform() ? event.metaKey && !event.ctrlKey : event.ctrlKey && !event.metaKey;
    if (!modifier || !activePageId) return;
    event.preventDefault();
    event.stopPropagation();
    void act('close', activePageId);
  }
</script>

<div
  class="flex h-full min-h-0 flex-col bg-surface-0"
  data-testid="browser-pane"
  onkeydowncapture={closeTabShortcut}
>
  <!-- svelte-ignore a11y_no_static_element_interactions -->
  <div
    class="flex min-h-9 items-end gap-1 overflow-x-auto border-b border-border-subtle bg-surface-1 px-1 pt-1"
    onmousedown={(event) => {
      // Middle-press would start OS autoscroll; on a tab strip it closes tabs.
      if (event.button === 1) event.preventDefault();
    }}
    ondragover={stripDragOver}
    ondrop={tabDrop}
  >
    {#if sessionName}
      <span class="mb-1 max-w-36 shrink-0 truncate px-1.5 text-[0.68rem] text-fg-muted" title={sessionName}>{sessionName}</span>
    {/if}
    {#each pages as page, tabIndex (page.id)}
      {#if dropSlot === tabIndex && dragPageId}
        <div class="h-6 w-0.5 shrink-0 self-center rounded bg-accent"></div>
      {/if}
      <div
        class="group flex h-8 min-w-28 max-w-48 items-center rounded-t-md {page.id === activePageId ? 'bg-surface-0 text-fg' : 'text-fg-muted hover:bg-surface-2'} {page.id === dragPageId ? 'opacity-50' : ''}"
        title={page.label ? `${page.label} — ${page.title || page.url}` : page.title || page.url}
        draggable="true"
        ondragstart={(event) => tabDragStart(event, page.id)}
        ondragover={(event) => tabDragOver(event, tabIndex)}
        ondrop={tabDrop}
        ondragend={tabDragEnd}
        onauxclick={(event) => {
          if (event.button !== 1) return;
          event.preventDefault();
          void act('close', page.id);
        }}
      >
        <button
          class="h-full min-w-0 flex-1 truncate pl-2 text-left text-[0.72rem]"
          onclick={() => void act('activate', page.id)}
        >{page.label || page.title || page.url || 'New tab'}</button>
        <button
          type="button"
          aria-label="Close tab"
          class="mr-1 rounded p-0.5 opacity-60 hover:bg-surface-3 hover:opacity-100"
          onclick={() => void act('close', page.id)}
        ><Icon icon={X} size={12} /></button>
      </div>
    {/each}
    {#if dropSlot === pages.length && dragPageId}
      <div class="h-6 w-0.5 shrink-0 self-center rounded bg-accent"></div>
    {/if}
    <button class="mb-1 rounded p-1.5 text-fg-muted hover:bg-surface-2 hover:text-fg" aria-label="New tab" title="New tab" onclick={() => void act('new', '')}>
      <Icon icon={Plus} size={14} />
    </button>
  </div>

  <div class="flex h-10 shrink-0 items-center gap-1 border-b border-border-subtle bg-surface-1 px-2">
    <button class="rounded p-1.5 text-fg-muted enabled:hover:bg-surface-2 enabled:hover:text-fg disabled:opacity-35" aria-label="Back" disabled={!activePage?.canGoBack} onclick={() => void act('back')}><Icon icon={ArrowLeft} size={14} /></button>
    <button class="rounded p-1.5 text-fg-muted enabled:hover:bg-surface-2 enabled:hover:text-fg disabled:opacity-35" aria-label="Forward" disabled={!activePage?.canGoForward} onclick={() => void act('forward')}><Icon icon={ArrowRight} size={14} /></button>
    <button class="rounded p-1.5 text-fg-muted hover:bg-surface-2 hover:text-fg" aria-label="Reload" onclick={() => void act('reload')}><Icon icon={RefreshCw} size={13} /></button>
    <input
      class="h-7 min-w-0 flex-1 rounded-md border border-border-subtle bg-surface-0 px-2 text-[0.75rem] text-fg outline-none focus:border-accent/60"
      aria-label="Address"
      bind:value={address}
      onfocus={() => (addressFocused = true)}
      onblur={() => (addressFocused = false)}
      onkeydown={(event) => {
        if (event.key === 'Enter') {
          event.preventDefault();
          (event.currentTarget as HTMLInputElement).blur();
          void act('navigate', activePageId, address);
        }
      }}
    />
    {#if activeIsLocalFile}
      <button
        class="rounded p-1.5 text-fg-muted hover:bg-surface-2 hover:text-fg"
        aria-label="Show in folder"
        title="Show in folder"
        onclick={() => void revealPageFile()}
      ><Icon icon={FolderOpen} size={13} /></button>
    {/if}
    <button
      class="rounded p-1.5 text-fg-muted hover:bg-surface-2 hover:text-fg"
      aria-label="Open devtools"
      title="Open devtools"
      onclick={() => void act('devtools')}
    ><Icon icon={SquareCode} size={13} /></button>
    <button class="rounded p-1.5 text-fg-muted hover:bg-surface-2 hover:text-fg" aria-label="Close browser" title="Close browser" onclick={() => void act('hide', '')}><Icon icon={X} size={14} /></button>
  </div>

  {#if error}
    <div class="shrink-0 border-b border-error/25 bg-error/10 px-3 py-1.5 text-[0.7rem] text-error" role="alert">{error}</div>
  {/if}

  <!-- The host rect. The native browser view is positioned exactly over this
       element by the platform host; what renders HERE is only visible when
       no view is presented (no engine, or the page is hidden). -->
  <div
    bind:this={surface}
    class="relative min-h-0 flex-1 overflow-hidden bg-surface-0"
    data-testid="browser-pane-host-rect"
    aria-label="Browser page"
  >
    {#if view?.error || attachment?.error}
      <div class="flex h-full items-center justify-center px-4 text-sm text-error">{view?.error || attachment?.error}</div>
    {/if}
  </div>
</div>
