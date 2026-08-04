// Keeps a listbox popover's active option visible under keyboard navigation
// without letting the mouse and the scroll fight each other.
//
// Two rules, one instance per popover:
//
//  - An activeIndex change the KEYBOARD made scrolls the active row into
//    view (`block: 'nearest'`, so an already-visible row never moves).
//  - An activeIndex change the MOUSE made must not scroll: the hovered row
//    is under the cursor and therefore already visible, and scrolling a
//    partially clipped row flush would shift the list under a stationary
//    pointer — the classic hover → scroll → new row hovered → scroll
//    cascade.
//
// A popover cannot see the source of a prop change, so the mouse path
// declares itself: row handlers call `hovered(index)` before forwarding to
// `onHover`, and `sync` — called from the popover's `$effect` after every
// DOM flush — skips exactly that one change. Rows activate on `mousemove`,
// not `mouseenter`, for the same reason from the other side: a keyboard
// scroll that slides a new row under a stationary cursor fires enter but
// not move, and must not steal the selection.
export interface ActiveOptionReveal {
  /** Call from a row's mousemove, before forwarding the index to onHover. */
  hovered(index: number): void;
  /**
   * Scroll the container's `aria-selected` option into view, unless the
   * change being synced is the one `hovered` just announced.
   */
  sync(activeIndex: number, container: HTMLElement | undefined): void;
}

export function createActiveOptionReveal(): ActiveOptionReveal {
  let hoverIndex: number | null = null;
  return {
    hovered(index: number): void {
      hoverIndex = index;
    },
    sync(activeIndex: number, container: HTMLElement | undefined): void {
      const mouseMadeThisChange = hoverIndex === activeIndex;
      hoverIndex = null;
      if (mouseMadeThisChange || !container) return;
      container
        .querySelector<HTMLElement>('[role="option"][aria-selected="true"]')
        ?.scrollIntoView({ block: 'nearest' });
    },
  };
}
