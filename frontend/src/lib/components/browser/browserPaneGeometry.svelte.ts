import { reportBrowserPaneRect } from '../../stores/browserCompanion.svelte';
import { airspaceIntersects, airspaceSurfaces } from '../../utils/paneAirspace.svelte';
import { promotedLayerClip, promotedLayerFrame } from '../../utils/panePromotion.svelte';
import { rgbChannels, toConcreteColor } from '../../utils/cssColorProbe';
import { getPaneLayoutItems } from '../../stores/paneLayout.svelte';
import { getSidebarWidth, isSidebarCollapsed } from '../../stores/sidebarLayout.svelte';
import { getAppliedTheme } from '../../theme/themeApply.svelte';

// ─── Host-rect geometry ────────────────────────────────────────────────
// At most one report per frame: every signal below only SCHEDULES, and the
// flush reads geometry once. There is deliberately no standing rAF loop —
// a per-frame callback would pin frame production at panel refresh.

export interface BrowserPaneGeometryOptions {
  /** The host rect element the native view is positioned over. */
  surface: () => HTMLElement | undefined;
  threadId: () => string | null;
  /** Whether the pane holds a companion view; a re-attach re-sends the rect. */
  attached: () => boolean;
}

export interface BrowserPaneGeometry {
  /** The host rect's own size, read off the same report as the geometry. */
  readonly hostWidth: number;
  readonly hostHeight: number;
}

/**
 * Reports the host rect's geometry for the calling pane. Call during
 * component init: the effects it creates belong to that component.
 */
export function createBrowserPaneGeometry(options: BrowserPaneGeometryOptions): BrowserPaneGeometry {
  let rectFrame = 0;
  let bgRaw = '';
  let bgHex = '';
  let hostWidth = $state(0);
  let hostHeight = $state(0);

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
    // A promoted pane reveals through an animated clip-path, which no
    // ancestor's overflow reports.
    const layer = promotedLayerClip(el);
    if (layer) {
      left = Math.max(left, layer.left);
      top = Math.max(top, layer.top);
      right = Math.min(right, layer.right);
      bottom = Math.min(bottom, layer.bottom);
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
    const el = options.surface();
    const threadId = options.threadId();
    if (!el || !threadId) return;
    const rect = el.getBoundingClientRect();
    hostWidth = rect.width;
    hostHeight = rect.height;
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
      !airspaceIntersects({ left: clip.left, top: clip.top, right: clip.right, bottom: clip.bottom }, el);
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
      // The SPA's own pixel ratio (DPI times webview zoom): the host draws the
      // page's CSS pixels at its own ratio, and the difference is the zoom the
      // presentation must carry so the page fills the fitted rect.
      devicePixelRatio: window.devicePixelRatio,
      visible,
      background: paneBackground(el),
    };
    // The store sends it in order and skips one the backend already has.
    reportBrowserPaneRect(threadId, report);
  }

  // Leaving a thread drops its pending frame.
  $effect(() => {
    if (!options.threadId()) return;
    return () => {
      if (rectFrame) cancelAnimationFrame(rectFrame);
      rectFrame = 0;
    };
  });

  $effect(() => {
    const el = options.surface();
    if (!el || !options.threadId()) return;
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
    options.attached();
    scheduleRect();
  });

  // A promoted pane, this one or one above it, changes what is visible here
  // on every frame of its reveal and when it starts or ends.
  $effect(() => {
    promotedLayerFrame();
    scheduleRect();
  });

  // Layout-driven POSITION changes: a divider drag on another pane slides
  // this pane sideways without resizing it, which no ResizeObserver, window
  // resize or scroll event reports (live incident 2026-08-31: the native
  // view stayed planted until the next scroll). Pane widths and the sidebar
  // are the app's own movers, so reading them here re-measures on every
  // frame of a drag. Over-firing is fine: the store skips an unchanged rect.
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

  return {
    get hostWidth() {
      return hostWidth;
    },
    get hostHeight() {
      return hostHeight;
    },
  };
}
