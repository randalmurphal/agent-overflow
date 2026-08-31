<script lang="ts">
  import ArrowLeft from '@lucide/svelte/icons/arrow-left';
  import ArrowRight from '@lucide/svelte/icons/arrow-right';
  import Copy from '@lucide/svelte/icons/copy';
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
    BrowserCompanionCopyPageFile,
    BrowserCompanionDo,
  } from '../../stores/bindings';
  import Icon from '../primitives/Icon.svelte';
  import { errString } from '../../utils/errors';
  import { airspaceIntersects, airspaceSurfaces } from '../../utils/paneAirspace.svelte';
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
  let lastSent = { x: -1, y: -1, width: -1, height: -1, viewportWidth: -1, viewportHeight: -1, visible: false };

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
      lastSent = { x: -1, y: -1, width: -1, height: -1, viewportWidth: -1, viewportHeight: -1, visible: false };
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

  // A native view cannot be partially cropped by the DOM, so a host rect
  // that pokes outside a clipping ancestor (the pane scrolled half behind
  // the sidebar) hides the view rather than letting it overhang neighbors.
  function clippedByAncestors(el: HTMLElement, rect: DOMRect): boolean {
    for (let node = el.parentElement; node; node = node.parentElement) {
      const style = getComputedStyle(node);
      if (style.overflowX === 'visible' && style.overflowY === 'visible') continue;
      const r = node.getBoundingClientRect();
      if (
        rect.left < r.left - 1 ||
        rect.right > r.right + 1 ||
        rect.top < r.top - 1 ||
        rect.bottom > r.bottom + 1
      ) {
        return true;
      }
    }
    return false;
  }

  function flushRect(): void {
    const el = surface;
    const threadId = ctx.threadId;
    if (!el || !threadId) return;
    const rect = el.getBoundingClientRect();
    const visible =
      rect.width >= 1 &&
      rect.height >= 1 &&
      !clippedByAncestors(el, rect) &&
      !airspaceIntersects(rect);
    const report = {
      x: Math.round(rect.left * 100) / 100,
      y: Math.round(rect.top * 100) / 100,
      width: Math.round(rect.width * 100) / 100,
      height: Math.round(rect.height * 100) / 100,
      viewportWidth: window.innerWidth,
      viewportHeight: window.innerHeight,
      visible,
    };
    if (
      report.x === lastSent.x &&
      report.y === lastSent.y &&
      report.width === lastSent.width &&
      report.height === lastSent.height &&
      report.viewportWidth === lastSent.viewportWidth &&
      report.viewportHeight === lastSent.viewportHeight &&
      report.visible === lastSent.visible
    ) {
      return;
    }
    lastSent = report;
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
    if (attached) lastSent = { ...lastSent, x: -1 };
    scheduleRect();
  });

  async function act(kind: string, pageId = activePageId, nextAddress = ''): Promise<void> {
    const threadId = ctx.threadId;
    if (!threadId) return;
    error = '';
    try {
      const state = await BrowserCompanionDo(threadId, new BrowserCompanionAction({
        kind,
        pageId,
        address: nextAddress,
      }));
      applyBrowserCompanionState(state);
    } catch (err) {
      error = errString(err);
    }
  }

  async function copyPageFile(): Promise<void> {
    const threadId = ctx.threadId;
    if (!threadId || !activePageId) return;
    error = '';
    try {
      await BrowserCompanionCopyPageFile(threadId, activePageId);
    } catch (err) {
      error = errString(err);
    }
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
  <div class="flex min-h-9 items-end gap-1 overflow-x-auto border-b border-border-subtle bg-surface-1 px-1 pt-1">
    {#if sessionName}
      <span class="mb-1 max-w-36 shrink-0 truncate px-1.5 text-[0.68rem] text-fg-muted" title={sessionName}>{sessionName}</span>
    {/if}
    {#each pages as page (page.id)}
      <div
        class="group flex h-8 min-w-28 max-w-48 items-center rounded-t-md {page.id === activePageId ? 'bg-surface-0 text-fg' : 'text-fg-muted hover:bg-surface-2'}"
        title={page.label ? `${page.label} — ${page.title || page.url}` : page.title || page.url}
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
    <button class="mb-1 rounded p-1.5 text-fg-muted hover:bg-surface-2 hover:text-fg" aria-label="New tab" title="New tab" onclick={() => void act('new', '')}>
      <Icon icon={Plus} size={14} />
    </button>
  </div>

  <div class="flex h-10 shrink-0 items-center gap-1 border-b border-border-subtle bg-surface-1 px-2">
    <button class="rounded p-1.5 text-fg-muted hover:bg-surface-2 hover:text-fg" aria-label="Back" onclick={() => void act('back')}><Icon icon={ArrowLeft} size={14} /></button>
    <button class="rounded p-1.5 text-fg-muted hover:bg-surface-2 hover:text-fg" aria-label="Forward" onclick={() => void act('forward')}><Icon icon={ArrowRight} size={14} /></button>
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
        aria-label="Copy file to clipboard"
        title="Copy file to clipboard"
        onclick={() => void copyPageFile()}
      ><Icon icon={Copy} size={13} /></button>
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
