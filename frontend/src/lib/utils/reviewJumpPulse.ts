import type { Attachment } from 'svelte/attachments';
import { motionReduced } from './reducedMotion';

// The review pane's jump emphasis: a jump to a thread (the unresolved
// stepper, a card's location, a row's "Open in conversation") pulses the
// destination card once and leaves it as it was. The reader is already
// looking at it, so nothing stays lit.
//
// The owner of a jump (ReviewDiffBody for rows, ReviewConversation for
// cards) holds the destination and a generation for REVIEW_JUMP_PULSE_MS
// and then forgets it; the card runs `jumpPulse` with that generation.
// A new generation on the same card restarts the pulse, so a repeat jump
// pulses again even mid pulse.

/** How long a jump destination is held by its owner; the pulse's lifetime. */
export const REVIEW_JUMP_PULSE_MS = 1600;

const RING_CLEAR = '0 0 0 2px transparent';

/**
 * Pulses the element once per non-zero `generation`: an accent ring at
 * full for the first third, fading out over the rest. Under reduced
 * motion (OS preference or low power) the ring holds unchanged and then
 * clears, so the emphasis is still seen, just not animated. A box-shadow
 * on the compositor: no layout. 0 means no jump.
 */
export function jumpPulse(generation: number): Attachment {
  return (el) => {
    if (generation === 0) return;
    // The theme's accent, resolved here rather than as var() inside the
    // keyframes, which not every engine's Web Animations accept.
    const accent = getComputedStyle(el).getPropertyValue('--color-accent').trim() || 'currentColor';
    const ring = `0 0 0 2px ${accent}`;
    const keyframes = motionReduced()
      ? [{ boxShadow: ring }, { boxShadow: ring }]
      : [{ boxShadow: ring, offset: 0 }, { boxShadow: ring, offset: 0.3 }, { boxShadow: RING_CLEAR, offset: 1 }];
    const animation = el.animate(keyframes, { duration: REVIEW_JUMP_PULSE_MS, easing: 'ease-out', fill: 'none' });
    return () => animation.cancel();
  };
}
