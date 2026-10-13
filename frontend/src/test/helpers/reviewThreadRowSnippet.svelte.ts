import { createRawSnippet } from 'svelte';
import type { CommentAnchor } from '../../lib/utils/reviewRows';

// A stand-in for ReviewDiffBody's comment-thread row: a fixed-height box
// whose `data-pulse` follows the snippet's pulse argument, so a test can
// read the generation the body hands the row as it changes.
export function reviewThreadRowSnippet(height = 80) {
  return createRawSnippet((threadKey: () => string, _anchor: () => CommentAnchor, pulse: () => number) => ({
    render: () => `<div data-testid="thread" data-thread="${threadKey()}" style="height:${height}px">${threadKey()}</div>`,
    setup: (node: Element) => {
      $effect(() => {
        (node as HTMLElement).dataset.pulse = String(pulse());
      });
    },
  }));
}
