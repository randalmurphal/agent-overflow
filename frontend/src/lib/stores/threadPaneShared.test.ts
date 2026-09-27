import { describe, expect, it, vi } from 'vitest';
import { stubScrollController } from '../../test/helpers/chat';
import {
  trimToTailRunes,
  withViewportBottomHeld,
  type PaneScrollController,
} from './threadPaneShared';

describe('withViewportBottomHeld', () => {
  it('hands the change to a controller that can hold the bottom edge', () => {
    const change = vi.fn();
    const hold = vi.fn((run: () => void) => run());

    withViewportBottomHeld(
      stubScrollController({ preserveViewportBottom: hold }),
      change,
    );

    expect(hold).toHaveBeenCalledTimes(1);
    expect(change).toHaveBeenCalledTimes(1);
  });

  it('still applies the change on a surface that cannot hold it', () => {
    // The load-bearing case. `controller?.preserveViewportBottom?.(change)`
    // reads as equivalent and is not: on a pane whose controller has no
    // virtualizer behind it — ChannelView's raw controller, or a pane whose
    // timeline has not mounted yet — it silently does nothing, and the run the
    // reader clicked simply never collapses.
    const change = vi.fn();

    withViewportBottomHeld(stubScrollController(), change);
    withViewportBottomHeld(null, change);

    expect(change).toHaveBeenCalledTimes(2);
  });

  it('calls the hold on its own controller', () => {
    // Extracted through `const hold = controller.preserveViewportBottom` and
    // called bare, `this` would be undefined inside an implementation that
    // reads its own state.
    let self: unknown = null;
    const ctrl: PaneScrollController = stubScrollController({
      preserveViewportBottom(this: unknown, run: () => void) {
        self = this;
        run();
      },
    });

    withViewportBottomHeld(ctrl, () => {});

    expect(self).toBe(ctrl);
  });
});

describe('trimToTailRunes', () => {
  it('keeps the last runes without splitting a surrogate pair', () => {
    expect(trimToTailRunes('ab😀c', 2)).toBe('😀c');
    expect(trimToTailRunes('ab😀c', 3)).toBe('b😀c');
    expect(trimToTailRunes('abc', 5)).toBe('abc');
  });

  // The reasoning reveal trims its previous summary plus each delta rather
  // than the whole text (threadRevealRouting.ts).
  it('trims incrementally to the same text as trimming the whole', () => {
    let state = 7;
    const random = (bound: number): number => {
      state = (Math.imul(state, 1664525) + 1013904223) >>> 0;
      return state % bound;
    };
    const pieces = ['word ', '😀', '𝑥', 'é', '\n', 'ab'];
    for (let run = 0; run < 40; run++) {
      const parts: string[] = [];
      for (let index = 0; index < 400; index++) parts.push(pieces[random(pieces.length)]);
      const text = parts.join('');
      const maxRunes = 1 + random(120);
      let trimmed = '';
      // Split at any code unit, including between the halves of a pair.
      for (let end = 0; end < text.length; ) {
        const next = Math.min(text.length, end + 1 + random(9));
        trimmed = trimToTailRunes(trimmed + text.slice(end, next), maxRunes);
        end = next;
        expect(trimmed).toBe(trimToTailRunes(text.slice(0, end), maxRunes));
      }
    }
  });
});
