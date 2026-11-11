// Where the reader is inside a virtual row, from its laid-out text: the
// line at the view's reading edge, wrapped lines included. A new find
// query starts from it (stores/reviewFind.svelte.ts `readingPosition`).

/** The caret at a viewport point, or null where the engine offers none. */
function caretAt(x: number, y: number): { node: Node; offset: number } | null {
  const doc = document as Document & {
    caretPositionFromPoint?(x: number, y: number): { offsetNode: Node; offset: number } | null;
    caretRangeFromPoint?(x: number, y: number): Range | null;
  };
  if (typeof doc.caretPositionFromPoint === 'function') {
    const at = doc.caretPositionFromPoint(x, y);
    return at ? { node: at.offsetNode, offset: at.offset } : null;
  }
  const range = doc.caretRangeFromPoint?.(x, y) ?? null;
  return range ? { node: range.startContainer, offset: range.startOffset } : null;
}

/**
 * The 0-based line of `element`'s newline-separated, pre-wrapped text
 * laid out at viewport y `edge`: the line whose text reaches below it.
 * Null when the point at the edge is not on the element's text.
 */
export function textLineAtEdge(element: HTMLElement, edge: number): number | null {
  const box = element.getBoundingClientRect();
  if (edge < box.top) return 0;
  if (edge >= box.bottom) return null;
  const caret = caretAt(box.left + Math.min(16, box.width / 2), edge + 0.5);
  if (!caret || !element.contains(caret.node)) return null;
  const before = document.createRange();
  before.setStart(element, 0);
  before.setEnd(caret.node, caret.offset);
  let lines = 0;
  for (const char of before.toString()) if (char === '\n') lines += 1;
  return lines;
}

/** The first of `elements` laid out across viewport y `edge`; null when
 * none is. */
export function elementAtEdge<E extends Element>(elements: Iterable<E>, edge: number): E | null {
  for (const element of elements) {
    const rect = element.getBoundingClientRect();
    if (rect.top <= edge && edge < rect.bottom) return element;
  }
  return null;
}
