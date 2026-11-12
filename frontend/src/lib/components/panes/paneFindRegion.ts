// The attachments that make an element a target of its pane's find
// commands (stores/paneFind.svelte.ts).
//
// `paneFindRegion` marks a declared region (`declarePaneFindRegions`):
// focus moving into it, or a press inside it, makes it the region find acts
// on, and it is the root a find seed is read from.
//
// `paneFindSurface` marks a nested surface with keys of its own
// (`declarePaneFindSurface`): focus or a press anywhere in the pane moves
// the commands into the surface or back out of it. Outside a pane element
// only the surface's own events count.

import type { Attachment } from 'svelte/attachments';
import {
  activatePaneFindRegion,
  declarePaneFindSurface,
  setPaneFindRegionElement,
} from '../../stores/paneFind.svelte';

export function paneFindRegion(key: string): Attachment<HTMLElement> {
  return (node) => {
    const activate = (): void => activatePaneFindRegion(key);
    node.addEventListener('focusin', activate);
    node.addEventListener('pointerdown', activate, true);
    const release = setPaneFindRegionElement(key, node);
    return () => {
      node.removeEventListener('focusin', activate);
      node.removeEventListener('pointerdown', activate, true);
      release();
    };
  };
}

export function paneFindSurface(paneId: string, key: () => string | null): Attachment<HTMLElement> {
  return (node) => {
    const root = node.closest<HTMLElement>('[data-pane-id]') ?? node;
    const active = node.ownerDocument.activeElement;
    const surface = declarePaneFindSurface(paneId, key, active !== null && node.contains(active));
    const track = (event: Event): void => {
      surface.setActive(event.target instanceof Node && node.contains(event.target));
    };
    root.addEventListener('focusin', track);
    root.addEventListener('pointerdown', track, true);
    return () => {
      root.removeEventListener('focusin', track);
      root.removeEventListener('pointerdown', track, true);
      surface.release();
    };
  };
}
