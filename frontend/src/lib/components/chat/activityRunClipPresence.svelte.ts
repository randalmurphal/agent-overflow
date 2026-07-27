// Whether an activity run has a clip on screen, and how it loses one.
//
// A run always draws its header; the clip under it is what comes and goes.
// Open means both, closed means the header alone, and `collapsed` is the whole
// answer — the registry has already folded liveness into it, so a live run
// nobody has answered for arrives here as an OPEN one and the reader's own
// collapse arrives as a closed one.
//
// That is what leaves one transition the reader did not ask for: such a run
// stops being live, its collapse falls back to the thread's default, and the
// clip has to go. Every other collapse in this area is an instant answer to a
// click, which is right — a state change the reader requested should land
// immediately. This one nobody requested, so it folds instead, and the fold is
// the whole reason this module exists.
//
// Split out of `ActivityRun.svelte` because it is a state machine with a
// lifetime: an animation, a deadline, a debt owed to a reader who is busy, and
// four ways to be interrupted. Inline it would be the largest thing in a file
// that is already mostly wiring.
//
// It owns no geometry beyond the height it starts from. The shrinking box is
// measured by the virtualizer's row observer like any other row, and what that
// means for the reading position is the scroll controller's decision — so a
// fold is one more layout change through the path every layout change already
// takes, not a second thing writing `scrollTop`.

import { tick } from 'svelte';
import {
  ACTIVITY_RUN_FOLD_EASING,
  activityRunFoldDeadlineMs,
  activityRunFoldDurationMs,
  prefersReducedMotion,
} from '../../utils/activityRunFold';

export interface ActivityRunClipPresenceOptions {
  /**
   * The run renders without its clip — `ActivityRunNode.collapsed`, which is
   * already liveness-aware. Not a raw reading of the reader's intent.
   */
  isCollapsed(): boolean;
  /**
   * The run holds the timeline's tail, so new activity lands in it. Read for
   * the fold's trigger only: losing the tail is what turns a clip nobody
   * collapsed into one that has to close.
   */
  isLive(): boolean;
  /**
   * The reader is somewhere inside the clip rather than resting on its newest
   * row. A fold would take the rows they are reading with it.
   */
  isReaderEngaged(): boolean;
  /** The box the fold animates. Undefined until the clip is mounted. */
  getFoldEl(): HTMLElement | undefined;
  /**
   * Report clip presence outward. The registry refuses to record an inner
   * scroll position for a run with no clip, and this is how it knows.
   */
  onClipOpenChange(open: boolean): void;
}

export interface ActivityRunClipPresence {
  /** The clip belongs in the DOM. */
  readonly open: boolean;
  /** A fold is in flight: the clip is pinned to the closing edge. */
  readonly folding: boolean;
  /** Height the fold started from, and the clip's height for its duration. */
  readonly foldFromPx: number;
}

export function createActivityRunClipPresence(
  options: ActivityRunClipPresenceOptions,
): ActivityRunClipPresence {
  // Seeded from the same rule the effect below applies, not from `false`. A
  // clip that appeared one flush after the row would make the row's mount
  // write — the one that puts a run on its newest activity — land a frame
  // late, on every run in the buffer.
  let open = $state(!options.isCollapsed());
  let folding = $state(false);
  let foldFromPx = $state(0);

  // Machinery, not render state: nothing draws from any of these.
  let animation: Animation | null = null;
  let deadline: ReturnType<typeof setTimeout> | null = null;
  /**
   * The run finished while the reader was inside it, so the fold is owed but
   * not yet allowed. It also distinguishes the two ways a clip closes: a debt
   * means the run ended on its own and animates, no debt means somebody
   * clicked and it is instant.
   */
  let foldOwed = false;
  let wasLive = false;

  // Unconditional rather than change-gated. A row remounting after eviction
  // starts with `open` already false and would report nothing, leaving the
  // registry holding the previous row's answer.
  function setOpen(next: boolean): void {
    open = next;
    options.onClipOpenChange(next);
  }
  options.onClipOpenChange(false);

  function endFold(): void {
    if (deadline !== null) {
      clearTimeout(deadline);
      deadline = null;
    }
    animation = null;
    folding = false;
    foldFromPx = 0;
  }

  /**
   * Abandon a fold, in flight or merely pending.
   *
   * `cancel()` routes through `oncancel`, so an animation that exists cleans
   * up on exactly one path whether it ended by itself or was interrupted. The
   * direct call covers the gap between `folding` going true and the animation
   * being constructed — one flush wide, and the reader can click through it.
   */
  function abortFold(): void {
    if (animation) {
      animation.cancel();
      return;
    }
    endFold();
  }

  async function startFold(): Promise<void> {
    const box = options.getFoldEl();
    if (!box) {
      setOpen(false);
      return;
    }
    const fromPx = box.offsetHeight;
    // Nothing to animate, nothing on screen to animate, or the reader asked
    // for no motion: close it the way a click would.
    if (fromPx <= 0 || !box.offsetParent || prefersReducedMotion()) {
      setOpen(false);
      return;
    }

    foldFromPx = fromPx;
    folding = true;
    // The clip only detaches from the flow — pinning it to the closing edge,
    // so the run shuts onto its NEWEST row and the older ones leave through
    // the top — once this state reaches the DOM. Animating before that would
    // fold a box whose content is still pushing it open.
    await tick();
    if (!folding) return;

    const durationMs = activityRunFoldDurationMs(fromPx);
    const anim = box.animate(
      [{ height: `${fromPx}px` }, { height: '0px' }],
      { duration: durationMs, easing: ACTIVITY_RUN_FOLD_EASING, fill: 'forwards' },
    );
    animation = anim;
    // A backgrounded tab stops advancing the animation but keeps firing
    // timers, so this is what guarantees the fold ends. `finish()` resolves
    // through `onfinish`, so the outcome is the same one the animation would
    // have produced — not a separate closing path that could drift from it.
    deadline = setTimeout(
      () => anim.finish(),
      activityRunFoldDeadlineMs(durationMs),
    );
    anim.onfinish = () => {
      if (animation !== anim) return;
      endFold();
      foldOwed = false;
      setOpen(false);
    };
    anim.oncancel = () => {
      if (animation !== anim) return;
      // Debt deliberately survives a cancel: the run is still finished, so if
      // the interruption was the reader stepping into the clip, the fold is
      // still owed once they step back out.
      endFold();
    };
  }

  $effect(() => {
    const collapsed = options.isCollapsed();
    const live = options.isLive();
    const engaged = options.isReaderEngaged();
    const justFinished = wasLive && !live;
    wasLive = live;

    // The clip belongs on screen. Wins over a fold in flight, which is how
    // expanding a run mid-fold reopens it instead of watching it finish
    // closing.
    if (!collapsed) {
      abortFold();
      foldOwed = false;
      setOpen(true);
      return;
    }
    if (folding || !open) return;
    if (justFinished) foldOwed = true;
    // Nobody is owed a fold, so this collapse came from a click — the rail,
    // the header, or the header bar's bulk toggle. Those are answers to the reader,
    // and an answer should not take 200ms to arrive.
    if (!foldOwed) {
      setOpen(false);
      return;
    }
    // Owed, but the reader is standing in it. The debt keeps until they leave
    // the clip's newest row alone; a run they never come back to simply stays
    // open, which is the one outcome that never moves anything under them.
    if (engaged) return;
    void startFold();
  });

  $effect(() => () => {
    // Null the handle FIRST so the cancel below finds its callbacks disarmed:
    // this runs while the component is being destroyed, and `$state` writes
    // from `endFold` would have nowhere to land.
    const anim = animation;
    animation = null;
    if (deadline !== null) {
      clearTimeout(deadline);
      deadline = null;
    }
    anim?.cancel();
  });

  return {
    get open() {
      return open;
    },
    get folding() {
      return folding;
    },
    get foldFromPx() {
      return foldFromPx;
    },
  };
}
