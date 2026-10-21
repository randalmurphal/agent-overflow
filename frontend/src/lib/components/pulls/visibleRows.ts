// A Svelte action that reports which rows of a scrolling list are on
// screen, by the value of one data attribute on each row. The page hands
// the keys to its store, which turns them into the backend's focus set
// (the rows whose detail is read). Recomputed on scroll, on resize and
// when rows come and go, coalesced to one pass per frame; geometry is read
// against the container's box, so a row hidden by the container's own
// overflow counts as off screen.

export interface VisibleRowsOptions {
  /** The data attribute (without `data-`) that names a row. */
  attr: string;
  onVisible: (keys: string[]) => void;
}

export function visibleRows(node: HTMLElement, options: VisibleRowsOptions): { update(next: VisibleRowsOptions): void; destroy(): void } {
  let current = options;
  let frame: number | null = null;
  let last = '';

  function measure(): void {
    frame = null;
    const box = node.getBoundingClientRect();
    const keys: string[] = [];
    for (const row of node.querySelectorAll<HTMLElement>(`[data-${current.attr}]`)) {
      const rect = row.getBoundingClientRect();
      if (rect.bottom <= box.top || rect.top >= box.bottom) continue;
      const key = row.dataset[toDatasetKey(current.attr)];
      if (key) keys.push(key);
    }
    const joined = keys.join('\u0000');
    if (joined === last) return;
    last = joined;
    current.onVisible(keys);
  }

  function schedule(): void {
    if (frame !== null) return;
    frame = requestAnimationFrame(measure);
  }

  node.addEventListener('scroll', schedule, { passive: true });
  const resize = typeof ResizeObserver === 'undefined' ? null : new ResizeObserver(schedule);
  resize?.observe(node);
  const mutations = typeof MutationObserver === 'undefined' ? null : new MutationObserver(schedule);
  mutations?.observe(node, { childList: true, subtree: true });
  schedule();

  return {
    update(next: VisibleRowsOptions): void {
      current = next;
      last = '';
      schedule();
    },
    destroy(): void {
      node.removeEventListener('scroll', schedule);
      resize?.disconnect();
      mutations?.disconnect();
      if (frame !== null) cancelAnimationFrame(frame);
    },
  };
}

// `data-pr-key` reads as `dataset.prKey`.
function toDatasetKey(attr: string): string {
  return attr.replace(/-([a-z])/g, (_, letter: string) => letter.toUpperCase());
}
