// DOM reads `find.in-pane` makes when it opens a pane's find bar: the pane's
// element, the text the reader selected inside it (the seed), and whether
// focus already sits in the bar.

/** Marks a find bar's root; a press from inside it keeps the bar's state. */
export const PANE_FIND_BAR_ATTR = 'data-pane-find-bar';

/** Longest selection that seeds a query; a longer one is not a search term. */
const MAX_SEED_LENGTH = 200;

export function paneElement(paneId: string): HTMLElement | null {
  if (typeof document === 'undefined') return null;
  return document.querySelector<HTMLElement>(`[data-pane-id="${CSS.escape(paneId)}"]`);
}

export function isInFindBar(element: Element | null): boolean {
  return element?.closest(`[${PANE_FIND_BAR_ATTR}]`) != null;
}

function usableSeed(text: string): string {
  const seed = text.trim();
  return seed && !seed.includes('\n') && seed.length <= MAX_SEED_LENGTH ? seed : '';
}

/** The single-line text selected inside `pane`, or ''. */
export function findSeedIn(pane: HTMLElement): string {
  const active = document.activeElement;
  if (isInFindBar(active)) return '';
  if ((active instanceof HTMLTextAreaElement || active instanceof HTMLInputElement) && pane.contains(active)) {
    const { selectionStart, selectionEnd, value } = active;
    if (selectionStart !== null && selectionEnd !== null && selectionEnd > selectionStart) {
      return usableSeed(value.slice(selectionStart, selectionEnd));
    }
    return '';
  }
  const selection = window.getSelection();
  if (!selection || selection.isCollapsed || selection.rangeCount === 0) return '';
  const range = selection.getRangeAt(0);
  if (!pane.contains(range.commonAncestorContainer)) return '';
  return usableSeed(selection.toString());
}
