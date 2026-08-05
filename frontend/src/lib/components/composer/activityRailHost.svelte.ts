// Host-side state for the ActivityRail: the background-tasks controller,
// the shared 1Hz clock, and the rail's visibility predicate. Owned by the
// HOST (Composer), not the rail, because visibility is load-bearing
// geometry there: the composer mounts the rail iff `railVisible` and
// renders its transparent height-reservation spacer as the exact
// complement, so exactly one of the two holds the row at all times and
// the composer's measured height — and the timeline's padding-bottom it
// drives via --composer-height — stays constant across turn
// start/complete, background-task end, and every other rail transition;
// the last message never jumps. Both branches flip in the same reactive
// flush, so there is no 1-frame double-height blip either.
//
// Do not re-derive "is the rail showing" anywhere else: the spacer once
// used its own predicate without the background term and stacked a
// phantom second row whenever a background task outlived its turn.
//
// Must be called from component init context (the clock uses runes).
// `mount()` subscribes the controller's event listeners; call it from the
// host's `onMount` and dispose the return value in `onDestroy`.

import type { ThreadPane } from '../../stores/thread.svelte';
import { getActiveTurn, isThreadWorking } from '../../stores/threadStatuses.svelte';
import {
  createSharedNowClock,
  type SharedNowClock,
} from '../chat/useRunningElapsed.svelte';
import {
  createBackgroundController,
  type BackgroundController,
} from './activityRailBackground.svelte';

export interface ActivityRailHost {
  readonly bg: BackgroundController;
  readonly clock: SharedNowClock;
  readonly railVisible: boolean;
  /** Subscribe the background controller; returns a disposer. */
  mount(): () => void;
}

export function createActivityRailHost(
  getPane: () => ThreadPane,
  /** Whether a pending user-input request is being shown (the rail's
   *  "Input requested" chip). Mirrors the `inputRequest` prop the host
   *  passes to the rail. */
  hasInputRequest: () => boolean,
): ActivityRailHost {
  // `bg` captures `clock` lazily; its deriveds first evaluate at render,
  // after both exist. Clock ownership mirrors what the rail renders: the
  // working elapsed label (suppressed while an input request blocks the
  // turn) and the background body's elapsed labels + completion-retention
  // prune.
  const bg = createBackgroundController(getPane, () => clock.now);
  const clock = createSharedNowClock(() => {
    const wantsClockForWorking =
      getActiveTurn(getPane().threadId) !== null && !hasInputRequest();
    const wantsClockForBackground =
      getPane().activityRailBackgroundOpen || bg.hasPendingCompletion;
    return wantsClockForWorking || wantsClockForBackground;
  });

  const railVisible = $derived(
    hasInputRequest() ||
      isThreadWorking(getPane().threadId) ||
      getPane().liveTodo !== null ||
      bg.count > 0,
  );

  return {
    get bg() { return bg; },
    get clock() { return clock; },
    get railVisible() { return railVisible; },
    mount: () => bg.mount(),
  };
}
