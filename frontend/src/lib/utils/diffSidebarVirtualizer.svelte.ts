// File-level virtualizer for the diff sidebar.
//
// IntersectionObserver-based — tracks each file's intersection with
// the scroll root expanded by a wide overscroll region (default
// 600px). Used by the file row to capture the last measured height
// for the (now-unreached) placeholder fallback.
//
// Earlier revisions also gated body rendering and Shiki worker
// dispatch on `visiblePaths`, but the observer turned out to be
// unreliable on initial mount and never re-fires for a fully-visible
// file when the user scrolls within it. Both gates were dropped:
// rendering happens on expand (see d382d68), tokenization happens
// on expand. The visible set is still maintained — it's the natural
// signal for future per-file dispatch priority — but no consumer
// currently gates correctness on it.

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
  // Svelte 5 tracks Set/Map mutations on $state — no need to clone
  // and reassign on every IntersectionObserver tick. With 50 files
  // crossing the threshold during a fast scroll, this saves dozens
  // of Set/Map allocations per second.
  const visible: Set<string> = $state(new Set());
  const heights: Map<string, number> = $state(new Map());
  let observer: IntersectionObserver | null = null;
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

  return {
    get visiblePaths(): ReadonlySet<string> {
      return visible;
    },
    init(root: HTMLElement, rootMargin = '600px 0px'): void {
      observer?.disconnect();
      observer = new IntersectionObserver(onIntersect, { root, rootMargin });
      // Re-observe any files that registered before init (component
      // mount order isn't guaranteed; the body sets root after the
      // file children mount via $effect).
      for (const el of elementsByPath.values()) {
        observer.observe(el);
      }
    },
    register(path: string, el: Element): void {
      elementsByPath.set(path, el);
      pathByElement.set(el, path);
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
      elementsByPath.clear();
    },
  };
}
