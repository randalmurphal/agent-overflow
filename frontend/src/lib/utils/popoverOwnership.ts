// Which surface owns an open popover, reconstructed from anchor chains.
//
// Popover portals every floating element to <body>, so DOM ancestry answers
// nothing about who spawned whom: a submenu and the menu that opened it are
// siblings, and a picker inside a dialog is a sibling of the dialog's
// backdrop. What survives portaling is the `__popoverAnchor` property each
// floating element carries — its anchor is still a DOM descendant of whatever
// opened it — so the chain of anchors is the only thing that can answer
// "does this popover belong to me?".
//
// Two consumers ask it, for the same reason (a key press belongs to the
// topmost surface that owns it): Popover, about its nested descendants, and
// Modal, about a picker mounted inside its panel.

/** A portaled popover's floating element, carrying its anchor reference. */
export type PopoverFloatingEl = HTMLElement & { __popoverAnchor?: HTMLElement };

// Real popover trees are rarely more than 2–3 deep; the cap is defensive
// against a malformed cycle, not a supported depth.
const MAX_CHAIN_DEPTH = 16;

/**
 * Whether `popover`'s anchor chain lands inside `container` — either
 * directly, or through however many intermediate popovers spawned it.
 */
export function popoverAnchorChainReaches(
  popover: PopoverFloatingEl,
  container: HTMLElement,
): boolean {
  let cur: PopoverFloatingEl | null = popover;
  for (let i = 0; i < MAX_CHAIN_DEPTH && cur; i++) {
    const anchor = cur.__popoverAnchor;
    if (!anchor) return false;
    if (container.contains(anchor)) return true;
    const next = anchor.closest('[data-popover]') as PopoverFloatingEl | null;
    if (!next || next === cur) return false;
    cur = next;
  }
  return false;
}

/**
 * Whether any open popover other than `container` itself is owned by it.
 *
 * The self-skip is what makes this safe to call with a floating element as
 * the container (Popover asking about its own descendants); a container that
 * is not a popover — a dialog panel — never matches it.
 */
export function hasOpenPopoverOwnedBy(container: HTMLElement): boolean {
  for (const popover of document.querySelectorAll<PopoverFloatingEl>('[data-popover]')) {
    if (popover === container) continue;
    if (popoverAnchorChainReaches(popover, container)) return true;
  }
  return false;
}
