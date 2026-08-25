// Explicit-jump session for MessageTimeline: the message-nav rail's
// click-to-jump flow, plus the landing flash that keeps an instant
// jump from disorienting (the viewport teleports; the flash says
// where it landed). The rail's ticks cover the whole thread, so an
// unloaded target is ordinary here — scrollToItem pages it in.
//
// The flash is an OVERLAY on MessageTimeline's non-scrolling wrapper,
// never a class on a row: rows run no CSS transitions (the timeline
// kill rule + Print Doctrine), and an overlay that fades outside the
// scroller needs no carve-out. It is positioned once at landing time
// and cancelled if the reader scrolls away — it does not track content.

import type {
  PaneSession,
} from '../../stores/threadPaneRoles';
import type { TimelineVirtualizerHandle } from '../../utils/virtual/types';
import { addToast } from '../../stores/toast.svelte';

/** Landing-flash geometry, viewport-relative to the timeline wrapper. */
export interface TimelineJumpFlash {
  top: number;
  height: number;
  /** Bumps per landing so a repeat jump restarts the CSS animation. */
  nonce: number;
}

/** Flash lifetime; keep in sync with the animation in MessageTimeline. */
export const JUMP_FLASH_DURATION_MS = 900;
/** A reader scroll beyond this many px from the landing kills the flash. */
const JUMP_FLASH_SCROLL_TOLERANCE_PX = 48;
/** Frames the landing wait will watch for the jump to converge. */
const JUMP_FLASH_MAX_WAIT_FRAMES = 45;
/** Consecutive same-offset frames that count as "the jump settled". */
const JUMP_FLASH_STABLE_FRAMES = 2;

export interface TimelineJumpOptions {
  getPane(): PaneSession;
  getListRef(): TimelineVirtualizerHandle | undefined;
  /**
   * Wired to the restore session's scrollToItem — the one jump path.
   * Resolves true only when the jump actually issued its scroll; a
   * refusal (item gone, superseded by a newer navigation) is false and
   * must not flash.
   */
  scrollToItem(id: string): Promise<boolean>;
  findTimelineNodeIndex(itemId: string): number;
}

export interface TimelineJump {
  /** Reactive — the overlay renders from this. */
  readonly flash: TimelineJumpFlash | null;
  /** Instant jump to a loaded-or-loadable item, then flash the landing. */
  jumpToItem(id: string): Promise<void>;
  /** Scroll-frame tap: cancels a flash the reader scrolled away from. */
  noteScroll(offset: number): void;
  /** Teardown: drop the pending flash timer. */
  invalidate(): void;
}

export function createTimelineJump(options: TimelineJumpOptions): TimelineJump {
  let flash: TimelineJumpFlash | null = $state(null);
  let flashNonce = 0;
  let flashAnchorOffset = 0;
  let flashTimer: ReturnType<typeof setTimeout> | undefined;
  let flashWaitFrame: number | undefined;

  function cancelFlashWait(): void {
    if (flashWaitFrame !== undefined) {
      cancelAnimationFrame(flashWaitFrame);
      flashWaitFrame = undefined;
    }
  }

  function clearFlash(): void {
    cancelFlashWait();
    if (flashTimer !== undefined) {
      clearTimeout(flashTimer);
      flashTimer = undefined;
    }
    flash = null;
  }

  // The jump's scrollToIndex is a multi-frame convergence — the write
  // re-targets as unmeasured rows around the destination resolve — so
  // the landing geometry cannot be read on the frame scrollToItem
  // resolved: the engine's offset is still pre-jump then, the target
  // computes as off-viewport, and the flash would silently never show.
  // Instead a bounded rAF watch waits for the offset to hold still for
  // a couple of frames WITH the target intersecting the viewport, and
  // positions the flash from that settled read. If the jump never
  // settles in view (reader grabbed the wheel mid-glide, thread
  // switched), the watch expires and no flash shows — correct both
  // times.
  function flashLanding(id: string): void {
    cancelFlashWait();
    // Item ids are thread-local and can recur across sibling/fork
    // threads, so the watch is pinned to the thread it was armed on.
    const threadId = options.getPane().threadId;
    let lastOffset = Number.NaN;
    let stableFrames = 0;
    let framesLeft = JUMP_FLASH_MAX_WAIT_FRAMES;
    const step = (): void => {
      flashWaitFrame = undefined;
      framesLeft -= 1;
      if (options.getPane().threadId !== threadId) return;
      const list = options.getListRef();
      if (!list) return;
      const retry = (): void => {
        if (framesLeft > 0) flashWaitFrame = requestAnimationFrame(step);
      };
      const idx = options.findTimelineNodeIndex(id);
      if (idx < 0) {
        retry();
        return;
      }
      const offset = list.getScrollOffset();
      const viewport = list.getViewportSize();
      if (viewport <= 0) {
        retry();
        return;
      }
      stableFrames = offset === lastOffset ? stableFrames + 1 : 0;
      lastOffset = offset;
      const rawTop = list.getItemOffset(idx) - offset;
      const rawHeight = list.sizeAt(idx);
      const inView = rawTop <= viewport && rawTop + rawHeight >= 0;
      if (stableFrames < JUMP_FLASH_STABLE_FRAMES || !inView) {
        retry();
        return;
      }
      // Clamp the rect to the viewport: a message taller than the
      // screen (or one straddling an edge) flashes what the reader can
      // see — the overlay must not paint past the wrapper into the
      // composer/header regions.
      const top = Math.max(0, rawTop);
      const height = Math.min(rawTop + rawHeight, viewport) - top;
      if (height <= 0) {
        retry();
        return;
      }
      flashAnchorOffset = offset;
      flashNonce += 1;
      if (flashTimer !== undefined) clearTimeout(flashTimer);
      flash = { top, height, nonce: flashNonce };
      flashTimer = setTimeout(() => {
        flashTimer = undefined;
        flash = null;
      }, JUMP_FLASH_DURATION_MS);
    };
    flashWaitFrame = requestAnimationFrame(step);
  }

  async function jumpToItem(id: string): Promise<void> {
    if (!id) return;
    let landed = false;
    try {
      landed = await options.scrollToItem(id);
    } catch (err) {
      console.error('Failed to jump to message:', err);
      addToast('error', 'Failed to jump to that message');
      return;
    }
    // A refused jump (superseded, item gone) must not flash — and must
    // not cancel the succeeding jump's own landing watch.
    if (landed) flashLanding(id);
  }

  function noteScroll(offset: number): void {
    if (flash === null) return;
    if (Math.abs(offset - flashAnchorOffset) > JUMP_FLASH_SCROLL_TOLERANCE_PX) {
      clearFlash();
    }
  }

  return {
    get flash() {
      return flash;
    },
    jumpToItem,
    noteScroll,
    invalidate: clearFlash,
  };
}
