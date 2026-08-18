// The popover cross-cutting contract: who owns an open popover, which
// surface clips it, and what a close reason licenses — the pieces of
// Popover's behaviour that its CALLERS and SIBLING surfaces participate in.
//
// Ownership is reconstructed from anchor chains. Popover portals every
// floating element to <body>, so DOM ancestry answers nothing about who
// spawned whom: a submenu and the menu that opened it are siblings, and a
// picker inside a dialog is a sibling of the dialog's backdrop. What
// survives portaling is the `__popoverAnchor` property each floating
// element carries — its anchor is still a DOM descendant of whatever
// opened it — so the chain of anchors is the only thing that can answer
// "does this popover belong to me?".
//
// Two consumers ask it, for the same reason (a key press belongs to the
// topmost surface that owns it): Popover, about its nested descendants, and
// Modal, about a picker mounted inside its panel. The clip boundary rides
// the same walk: a popover clips where its chain's REAL trigger clips.

/** A portaled popover's floating element, carrying its anchor reference. */
export type PopoverFloatingEl = HTMLElement & { __popoverAnchor?: HTMLElement };

/**
 * Why a Popover invoked `onClose`. Dismissals aimed at the popup itself
 * ('escape', 'tab') warrant the caller's focus restore; closes caused by
 * the user engaging something ELSE ('outside-click') or by the trigger
 * scrolling away / unmounting ('anchor-gone') must leave focus — and
 * with it, scroll — where the user put it. A caller invoking its own
 * close handler directly (item selection, trigger toggle, chord) passes
 * no reason, which counts as an explicit dismissal.
 */
export type PopoverCloseReason = 'outside-click' | 'escape' | 'tab' | 'anchor-gone';

/**
 * Whether a close with this reason licenses the caller to move focus (to
 * the composer, the trigger, wherever the caller restores to). An
 * allow-list on purpose: a future reason added to the union fails the
 * exhaustive switch here instead of silently inheriting "restore", which
 * is the dangerous default — a wrong restore steals focus AND, through
 * focus-follows-scroll, can yank the pane strip.
 */
export function popoverCloseRestoresFocus(reason: PopoverCloseReason | undefined): boolean {
  switch (reason) {
    case undefined: // caller-initiated: selection, trigger toggle, chord
    case 'escape':
    case 'tab':
      return true;
    case 'outside-click':
    case 'anchor-gone':
      return false;
  }
}

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
 * The clip boundary governing a popover's floating element: the nearest
 * `[data-popover-clip-boundary]` ancestor of its ANCHOR, walked across
 * portal hops the same way ownership is — a submenu whose anchor lives
 * inside another popover's floating element inherits the boundary of the
 * popover chain's real trigger. The pane strip declares the attribute so
 * popups opened from strip content clip at the strip's edge (behind the
 * sidebar) exactly like their triggers do, instead of floating over
 * sibling surfaces; popovers with no boundary ancestor are unclipped.
 *
 * The value `"none"` is a terminator: a surface that sits INSIDE a
 * boundary's subtree but escapes its scrolling — Modal's fixed panel,
 * OverlayShell's card — declares it so pickers it hosts stay unclipped
 * instead of inheriting a strip edge their trigger never scrolls behind.
 */
export function resolvePopoverClipBoundary(anchor: HTMLElement): HTMLElement | null {
  let node: HTMLElement | null = anchor;
  for (let i = 0; i < MAX_CHAIN_DEPTH && node; i++) {
    const boundary = node.closest<HTMLElement>('[data-popover-clip-boundary]');
    if (boundary) {
      return boundary.getAttribute('data-popover-clip-boundary') === 'none' ? null : boundary;
    }
    const host = node.closest('[data-popover]') as PopoverFloatingEl | null;
    if (!host?.__popoverAnchor || host.__popoverAnchor === node) return null;
    node = host.__popoverAnchor;
  }
  return null;
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
