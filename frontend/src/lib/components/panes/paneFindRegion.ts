// The attachment that makes an element a pane's find region
// (stores/paneFind.svelte.ts, `declarePaneFindRegions`): focus moving into
// it, or a press inside it, makes it the region find acts on, and it is the
// root a find seed is read from.

import type { Attachment } from 'svelte/attachments';
import { activatePaneFindRegion, setPaneFindRegionElement } from '../../stores/paneFind.svelte';

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
