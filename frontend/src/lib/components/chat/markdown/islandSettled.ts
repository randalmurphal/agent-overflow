import { RENDER_FAILED_ATTR } from '../../../markdown/render/elements/SourceText.svelte';

/**
 * Calls `settled` once `root` holds what its island's renderer drew
 * (`drawnSelector`) or the source the renderer shows after failing
 * (`RENDER_FAILED_ATTR`): either way the host's pending source fallback
 * gives way to the renderer's box. Returns the cleanup.
 */
export function watchIslandSettled(root: HTMLElement, drawnSelector: string, settled: () => void): () => void {
  const selector = `${drawnSelector}, [${RENDER_FAILED_ATTR}]`;
  if (root.querySelector(selector)) {
    settled();
    return () => {};
  }
  const observer = new MutationObserver(() => {
    if (!root.querySelector(selector)) return;
    observer.disconnect();
    settled();
  });
  observer.observe(root, { childList: true, subtree: true, attributeFilter: [RENDER_FAILED_ATTR] });
  return () => observer.disconnect();
}
