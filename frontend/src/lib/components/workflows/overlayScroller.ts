// Who owns the overlay's scroll container, stated once (RUN-MAP §9.9).
//
// ONE scroller serves every level of the workflows overlay, and it is an
// ANCESTOR of the run map rather than the map's own element. The map still has
// to write to it (placement, jump, follow, compensation — §9.1), so the
// element has to reach it somehow, and there are only two ways: the map walks
// up the DOM looking for something that scrolls, or the frame that owns the
// scroller hands it down.
//
// It is handed down. A DOM walk asks the layout a question the code already
// knows the answer to, silently picks up whatever `overflow-y` a future
// wrapper introduces between the two, and answers `null` for a map mounted
// somewhere it was never meant to be — which is exactly the case that must be
// loud. Context is a stated contract instead: the overlay provides, the map
// requires, and a map without a provider fails at mount with a message naming
// the fix.
//
// The value is a GETTER, not the element: `bindings` land after the frame's
// first render, and a captured null would never heal.

import { getContext, setContext } from 'svelte';

const KEY = Symbol('workflows-overlay-scroller');

export type OverlayScrollerRef = () => HTMLElement | null;

/** Called by the overlay frame during its own setup. */
export function setWorkflowsOverlayScroller(ref: OverlayScrollerRef): void {
  setContext(KEY, ref);
}

/**
 * The overlay body, for a component mounted inside it. Throws when there is no
 * provider — a run map outside the overlay has no scroller to write to, and a
 * silent null would present as follow and placement quietly not working.
 */
export function requireWorkflowsOverlayScroller(): OverlayScrollerRef {
  const ref = getContext<OverlayScrollerRef | undefined>(KEY);
  if (typeof ref !== 'function') {
    throw new Error(
      'workflows overlay scroller context is missing: mount this component inside '
        + 'WorkflowsOverlay.svelte, or call setWorkflowsOverlayScroller() in whatever '
        + 'frame owns the scroll container (RUN-MAP §9.9)',
    );
  }
  return ref;
}

/** Test seam: the context key, so a component test can mount with a provider. */
export const WORKFLOWS_OVERLAY_SCROLLER_KEY: symbol = KEY;
