// Whether the surface a write's control is on is shown to the person: the
// Board, Backlog and ticket screens each set it for the controls under
// them, and a control registers as drawing its target with it
// (`stores/trackerTicketWrites.svelte.ts#drawTicketWrite`, the rule of
// where an outcome shows).

import { getContext, setContext } from 'svelte';

const SURFACE = Symbol('ticket-write-surface');

/** Called by a surface during its init: `shown` answers whether the
 * person sees it now. */
export function setTicketWriteSurface(shown: () => boolean): void {
  setContext(SURFACE, shown);
}

/** The shown test of the surface a control is under; called during the
 * control's init. */
export function ticketWriteSurface(): () => boolean {
  const shown = getContext<(() => boolean) | undefined>(SURFACE);
  if (!shown) throw new Error('A ticket write control was mounted outside the Board, Backlog or ticket screen, so nothing says whether it is shown.');
  return shown;
}

/** The context a surface sets, for mounting one of its controls alone
 * (`mount`'s `context`). */
export function ticketWriteSurfaceContext(shown: () => boolean): Map<symbol, unknown> {
  return new Map([[SURFACE, shown]]);
}
