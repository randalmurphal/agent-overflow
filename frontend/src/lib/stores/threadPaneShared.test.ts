import { describe, expect, it, vi } from 'vitest';
import { stubScrollController } from '../../test/helpers/chat';
import { withViewportBottomHeld, type PaneScrollController } from './threadPaneShared';

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
