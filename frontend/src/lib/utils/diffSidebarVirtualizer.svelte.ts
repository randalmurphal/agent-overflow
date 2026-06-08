import { SvelteMap, SvelteSet } from 'svelte/reactivity';

// File-level virtualizer for the diff sidebar.
//
// IntersectionObserver-based file-level virtualizer for the diff sidebar.
// It tracks each file's intersection with the scroll root expanded by a wide
// overscroll region. Registration also runs a synchronous geometry pass so the
// first viewport is visible before the browser delivers the first observer
// callback.

export interface FileVirtualizerHandle {
  /** Reactive set of currently-visible (or near-visible) file paths. */
  readonly visiblePaths: ReadonlySet<string>;
  init(root: HTMLElement, rootMargin?: string): void;
  register(path: string, el: Element): void;
  unregister(path: string): void;
  isVisible(path: string): boolean;
  height(path: string): number | undefined;
  destroy(): void;
}

export function createFileVirtualizer(): FileVirtualizerHandle {
  const visible = new SvelteSet<string>();
  const heights = new SvelteMap<string, number>();
  let observer: IntersectionObserver | null = null;
  let rootEl: HTMLElement | null = null;
  let rootMarginPx = 600;
  const elementsByPath = new Map<string, Element>();
  const pathByElement = new WeakMap<Element, string>();

  function onIntersect(entries: IntersectionObserverEntry[]): void {
    for (const entry of entries) {
      const path = pathByElement.get(entry.target);
      if (!path) continue;
      if (entry.isIntersecting) {
        visible.add(path);
        const h = entry.boundingClientRect.height;
        if (h > 0) heights.set(path, h);
      } else {
        visible.delete(path);
      }
    }
  }

  function measureVisibility(path: string, el: Element): void {
    if (!rootEl) return;
    const rootRect = rootEl.getBoundingClientRect();
    const rect = el.getBoundingClientRect();
    const intersects =
      rect.bottom >= rootRect.top - rootMarginPx
      && rect.top <= rootRect.bottom + rootMarginPx;
    if (intersects) {
      visible.add(path);
      if (rect.height > 0) heights.set(path, rect.height);
    } else {
      visible.delete(path);
    }
  }

  function parseVerticalRootMargin(rootMargin: string): number {
    const first = rootMargin.trim().split(/\s+/)[0] ?? '';
    if (!first.endsWith('px')) return 600;
    const value = Number.parseFloat(first);
    return Number.isFinite(value) ? value : 600;
  }

  return {
    get visiblePaths(): ReadonlySet<string> {
      return visible;
    },
    init(root: HTMLElement, rootMargin = '600px 0px'): void {
      rootEl = root;
      rootMarginPx = parseVerticalRootMargin(rootMargin);
      observer?.disconnect();
      observer = new IntersectionObserver(onIntersect, { root, rootMargin });
      // Re-observe any files that registered before init (component
      // mount order isn't guaranteed; the body sets root after the
      // file children mount via $effect).
      for (const [path, el] of elementsByPath) {
        measureVisibility(path, el);
        observer.observe(el);
      }
    },
    register(path: string, el: Element): void {
      elementsByPath.set(path, el);
      pathByElement.set(el, path);
      measureVisibility(path, el);
      observer?.observe(el);
    },
    unregister(path: string): void {
      const el = elementsByPath.get(path);
      if (el) {
        observer?.unobserve(el);
        pathByElement.delete(el);
      }
      elementsByPath.delete(path);
      visible.delete(path);
    },
    isVisible(path: string): boolean {
      return visible.has(path);
    },
    height(path: string): number | undefined {
      return heights.get(path);
    },
    destroy(): void {
      observer?.disconnect();
      observer = null;
      rootEl = null;
      elementsByPath.clear();
      visible.clear();
    },
  };
}
