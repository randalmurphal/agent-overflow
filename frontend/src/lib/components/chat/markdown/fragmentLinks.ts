// Same-document links in a forge body (`[jump](#heading)`, a table of
// contents entry, a footnote back-reference). The renderer marks them
// `data-md-fragment` and marks their targets `data-md-anchor` instead of
// `id`: a pane shows many bodies, so a name is resolved inside the
// `.markdown-body` the link was clicked in and never reaches another body,
// the page's location or a new tab.
//
// The jump moves the scroll container around that body, never one further
// out: in the review pane that is the section's own scroller, which records
// the place the jump lands on through its scroll event (a review list row
// is never scrolled into view; review/AGENTS.md). Agent chat renders these
// links as plain references: the timeline's motion has one owner
// (frontend-scroll.md), and nothing in a transcript names these anchors.

let installed = false;

/** Installs the document-level handler once per page. */
export function ensureFragmentLinkDelegate(): void {
  if (installed || typeof document === 'undefined') return;
  installed = true;
  document.addEventListener('click', handleFragmentClick);
  document.addEventListener('auxclick', handleFragmentAuxClick);
}

function fragmentLink(target: EventTarget | null): HTMLAnchorElement | null {
  if (!(target instanceof Element)) return null;
  return target.closest<HTMLAnchorElement>('a[data-md-fragment]');
}

function handleFragmentClick(event: MouseEvent): void {
  if (event.defaultPrevented || event.button !== 0) return;
  const link = fragmentLink(event.target);
  if (!link) return;
  event.preventDefault();
  jumpToFragment(link);
}

// A middle click would open the page itself in a new tab.
function handleFragmentAuxClick(event: MouseEvent): void {
  if (fragmentLink(event.target)) event.preventDefault();
}

/** The anchor `name` names inside `root`, if any. */
export function fragmentTarget(root: Element, name: string): HTMLElement | null {
  let decoded = name;
  try {
    decoded = decodeURIComponent(name);
  } catch {
    // A malformed escape names itself.
  }
  return root.querySelector<HTMLElement>(`[data-md-anchor="${CSS.escape(decoded)}"]`);
}

/** Scrolls the target `link` names to the top of its body's scroll
 * container. Returns whether there was a target. */
export function jumpToFragment(link: HTMLAnchorElement): boolean {
  const root = link.closest('.markdown-body');
  const href = link.getAttribute('href') ?? '';
  if (!root || !href.startsWith('#')) return false;
  const target = fragmentTarget(root, href.slice(1));
  if (!target) return false;
  const scroller = scrollContainerOf(root);
  if (scroller) scroller.scrollTop += target.getBoundingClientRect().top - scroller.getBoundingClientRect().top;
  return true;
}

function scrollContainerOf(root: Element): HTMLElement | null {
  for (let node = root.parentElement; node; node = node.parentElement) {
    const overflow = getComputedStyle(node).overflowY;
    if (overflow === 'auto' || overflow === 'scroll' || overflow === 'overlay') return node;
  }
  return null;
}
