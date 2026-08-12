// Scroll/follow controller for the run map (RUN-MAP.md §9, "the
// intentionality contract").
//
// The doctrine is the timeline's, deliberately re-derived rather than
// imported: `utils/scroll/` is virtualizer- and spring-shaped, and the
// map is one short document with one moving point of interest. What
// carries over verbatim is the part that is product requirement rather
// than machinery:
//
//   - ONE write chokepoint. Every scrollTop write in the map goes
//     through `writeScrollTop` with a cause the user could name:
//     'place' (open), 'jump' (chip click), 'follow' (frontier moved),
//     'compensate' (net-zero anchor hold). Anything else is a bug.
//   - Escape is EVENT-SOURCED, never geometry-inferred
//     (`utils/scroll/intent.ts:9-11`). Only wheel/key/touch/pointer
//     escape follow. `scroll` events never do: a programmatic write and
//     a finger produce the same event, so geometry can never be
//     evidence of intent. This is what makes a follow glide incapable
//     of false-escaping itself.
//   - Re-engage is EXPLICIT ONLY (§9.3). There is no "you scrolled back
//     near the frontier so we grabbed you" path, deliberately stricter
//     than the timeline's bottom-restick — the map's follow target sits
//     mid-content and moves, so an implicit restick would be exactly the
//     force-grab the requirement forbids.
//   - `will-change` is never toggled here (§9.11, post-incident doctrine
//     at `utils/scroll/chokepoint.ts:179-199`): three visible-flicker
//     incidents traced to promote/demote transitions. If the glide ever
//     needs compositing it gets a static class in markup, not a lease.
//
// No springs, no residue, no token ring: the glide is a 250ms rAF ease
// and the only writer that runs across frames.
//
// The rect arithmetic all of this decides on lives next door in
// `runMapGeometry.ts`, pure and directly tested. This module is the state
// machine over it.

import { flushSync } from 'svelte';
import { motionReduced } from '../../utils/reducedMotion';
import {
  BAND_REST_FRACTION,
  canScroll,
  hasSelectionInside,
  inBand,
  isOffscreen,
  maxScrollTop,
  pickAnchor,
  restingScrollTop,
} from './runMapGeometry';

export { BAND_REST_FRACTION };

export const GLIDE_DURATION_MS = 250;
/** §9.12 rate contract: end-to-start spacing for resize recomputation. */
export const RESIZE_MIN_INTERVAL_MS = 100;
/** Sub-pixel writes are noise; both thresholds are "would the reader see it". */
const MIN_GLIDE_DISTANCE_PX = 1;
const MIN_ANCHOR_DELTA_PX = 0.5;

/**
 * Frames `attach` will wait for the scroller element before it gives up.
 *
 * The scroller is bound in an ANCESTOR frame (§9.9) and handed down as a
 * getter, so the honest answer to "is it there yet" at the moment a child
 * effect first runs is "today, yes" — Svelte binds parents before it runs
 * child effects. That is Svelte's ordering, not a contract this module can
 * state, and the failure it would leave behind is the one §9.1 and §9.2
 * forbid outright: `writeScrollTop` keeps writing through the live getter
 * while NO input listener is installed, so follow runs with no way for the
 * reader to escape it. Waiting a few frames heals a late bind; exhausting
 * them is a wiring bug, and it says so rather than presenting as "follow is
 * stuck on".
 */
const ATTACH_MAX_FRAMES = 3;

const UP_KEYS: ReadonlySet<string> = new Set(['PageUp', 'ArrowUp', 'Home']);

function noop(): void {}

export type RunMapScrollCause = 'place' | 'jump' | 'follow' | 'compensate';

export interface RunMapScrollWrite {
  readonly top: number;
  readonly cause: RunMapScrollCause;
}

export interface RunMapFollowDeps {
  /** The overlay body scroll container. */
  scroller: () => HTMLElement | null;
  /** DOM element of the current follow target; null before it renders. */
  followTargetEl: () => HTMLElement | null;
  /** True when the run opened in a running state (§9.4). */
  followDefault: () => boolean;
  /**
   * Observer over the write chokepoint, PER CONTROLLER. The controller's tests
   * assert the cause sequence through it, rather than reverse-engineering
   * intent from numbers a fake scroller saw — which is precisely the inference
   * this controller refuses to make about the user.
   *
   * Deliberately an instance dependency and not a module-level seam: two
   * controllers alive at once (a run detail remounting, two suites in one
   * worker) would interleave into one log, and a seam nobody reset would leak
   * across files.
   */
  onWrite?: (write: RunMapScrollWrite) => void;
}

export interface RunMapFollow {
  /** Reactive: is follow tracking the frontier right now. */
  readonly engaged: boolean;
  /** Reactive: show the `now ▸` chip (disengaged, or target off-screen). */
  readonly chipVisible: boolean;
  /** Chip click: engage follow and glide the target into view. */
  engage(): void;
  /** §9.5 — pre-paint placement. Never animates, whatever motion says. */
  placeOnOpen(): void;
  /** Call when the follow target key changes (frontier advanced, wave folded). */
  onFollowTargetChanged(): void;
  /** §9.7 — run a map-initiated layout mutation holding the reader's anchor. */
  holdAnchor<T>(mutate: () => T): T;
  /**
   * §9.7 for a layout change this controller cannot wrap: the store swaps the
   * whole view and Svelte applies the new layout on its own flush. Measure in
   * an `$effect.pre` (before the DOM changes) and call the returned function in
   * the matching `$effect` (after) — same anchor, same compensation write, same
   * "engaged owns the viewport" rule as `holdAnchor`, which is built on it.
   *
   * Deliberately NOT `holdAnchor(() => …)` at those call sites: `holdAnchor`
   * flushes synchronously, and a `flushSync` raised from inside an effect
   * re-enters the batch that is already flushing.
   *
   * The returned release is SINGLE-SHOT and bound to the world it measured:
   * calling it twice, or after follow engaged, or after the controller was
   * re-attached or the scroller swapped, writes nothing. A compensation is a
   * statement about a specific layout delta, and none of those worlds is the
   * one it measured.
   */
  captureAnchor(): () => void;
  /**
   * Install input listeners + the resize observer; returns cleanup.
   *
   * Never yields a dead installation: if the scroller has not rendered yet it
   * retries on the next frames and THROWS once `ATTACH_MAX_FRAMES` are spent.
   * The returned cleanup is generation-guarded, so a stale one cannot tear down
   * a newer attachment.
   */
  attach(): () => void;
}

export function createRunMapFollow(deps: RunMapFollowDeps): RunMapFollow {
  let engaged = $state(false);
  let chipVisible = $state(false);

  let glideFrame: number | null = null;
  let glideStartTs: number | null = null;
  let glideFrom = 0;
  let glideTo = 0;
  let glideCause: RunMapScrollCause = 'follow';

  /** The one pending chip refresh; scroll events coalesce onto it. */
  let scrollFrame: number | null = null;

  let touchY: number | null = null;

  let attachedEl: HTMLElement | null = null;
  let attachFrame: number | null = null;
  /**
   * Bumped by every teardown, and therefore by every attach (which begins with
   * one). It is the identity of "this installation", and it is what lets a
   * cleanup closure and an anchor release tell the world they were made for
   * from the one that replaced it.
   */
  let lifecycle = 0;
  let resizeObserver: ResizeObserver | null = null;
  let resizeCooldown: ReturnType<typeof setTimeout> | null = null;
  let resizePending = false;
  /**
   * Latched when `attach()` spent its frames without a scroller to listen on.
   *
   * The throw alone was loud but inert: it unwinds a rAF callback, and the
   * chokepoint reaches the element through `deps.scroller()` rather than
   * through anything the failed install owned — so follow could still engage
   * and glide with not one input listener attached, which IS §9.2's
   * inescapable follow, the state the throw's own message names. The latch is
   * what makes the message true: no writes, no engagement, no chip.
   *
   * Cleared by a successful `install`, so a scroller that arrives late (a
   * remount, a re-attach after the frame binds) recovers completely.
   */
  let installFailed = false;

  // ===== The write chokepoint =====

  function writeScrollTop(top: number, cause: RunMapScrollCause): void {
    if (installFailed) return;
    const el = deps.scroller();
    if (!el) return;
    const next = Math.max(0, Math.min(maxScrollTop(el), top));
    if (!Number.isFinite(next)) return;
    el.scrollTop = next;
    deps.onWrite?.({ top: next, cause });
  }

  // ===== Glide =====

  function cancelGlide(): void {
    if (glideFrame !== null) {
      cancelAnimationFrame(glideFrame);
      glideFrame = null;
    }
    glideStartTs = null;
  }

  function onGlideFrame(ts: number): void {
    glideFrame = null;
    const el = deps.scroller();
    if (!el) {
      glideStartTs = null;
      return;
    }
    // First frame only establishes the time base. Writing at t=0 would
    // re-write the position we already hold — a write with no motion.
    if (glideStartTs === null) {
      glideStartTs = ts;
      glideFrame = requestAnimationFrame(onGlideFrame);
      return;
    }
    const t = Math.min(1, Math.max(0, (ts - glideStartTs) / GLIDE_DURATION_MS));
    const eased = 1 - (1 - t) ** 3;
    writeScrollTop(glideFrom + (glideTo - glideFrom) * eased, glideCause);
    if (t < 1) {
      glideFrame = requestAnimationFrame(onGlideFrame);
      return;
    }
    glideStartTs = null;
    refreshChip();
  }

  /**
   * Start — or retarget — the one glide. A second call mid-flight rebases
   * the same rAF chain onto the new destination (§9.6: retargets, never
   * queues), so two frontier moves in one animation produce one motion.
   */
  function startGlide(el: HTMLElement, desired: number, cause: RunMapScrollCause): void {
    if (Math.abs(desired - el.scrollTop) < MIN_GLIDE_DISTANCE_PX) {
      cancelGlide();
      refreshChip();
      return;
    }
    glideTo = desired;
    glideCause = cause;
    if (motionReduced()) {
      cancelGlide();
      writeScrollTop(desired, cause);
      refreshChip();
      return;
    }
    glideFrom = el.scrollTop;
    glideStartTs = null;
    if (glideFrame === null) glideFrame = requestAnimationFrame(onGlideFrame);
  }

  /**
   * The one follow decision, shared by every trigger that can move the
   * frontier under the reader (target change, container resize): glide
   * only while engaged, only when the target has left the band, and
   * never out from under a live selection.
   */
  function followBandCheck(): void {
    const el = deps.scroller();
    const target = deps.followTargetEl();
    if (!engaged || !el || !target) return;
    if (inBand(el, target) || hasSelectionInside(el)) return;
    startGlide(el, restingScrollTop(el, target), 'follow');
  }

  // ===== Chip =====

  function refreshChip(): void {
    // A chip offering to re-engage a follow that cannot write, on a surface
    // with no listeners, is an affordance for nothing.
    if (installFailed) {
      chipVisible = false;
      return;
    }
    const target = deps.followTargetEl();
    if (!target) {
      chipVisible = false;
      return;
    }
    if (!engaged) {
      chipVisible = true;
      return;
    }
    const el = deps.scroller();
    chipVisible = el !== null && isOffscreen(el, target);
  }

  // ===== Escape (event-sourced only) =====

  function escape(): void {
    const el = deps.scroller();
    // Nothing to escape from on a surface that cannot scroll — and the
    // chip that would appear would be pure noise.
    if (!el || !canScroll(el) || !engaged) return;
    engaged = false;
    cancelGlide();
    refreshChip();
  }

  function onWheel(e: WheelEvent): void {
    if (e.ctrlKey || e.deltaY === 0) return;
    if (e.deltaY < 0) escape();
  }

  function onKeydown(e: KeyboardEvent): void {
    if (UP_KEYS.has(e.key)) escape();
  }

  function onTouchStart(e: TouchEvent): void {
    touchY = e.touches[0]?.clientY ?? null;
  }

  function onTouchMove(e: TouchEvent): void {
    if (touchY === null) return;
    const y = e.touches[0]?.clientY ?? touchY;
    const dy = y - touchY;
    touchY = y;
    // Finger DOWN (dy > 0) scrolls the page UP: the reader is going back
    // for context above the frontier. Same sign convention as
    // `utils/scroll/intent.ts` handleTouchMove.
    if (dy > 1) escape();
  }

  function onTouchEnd(): void {
    touchY = null;
  }

  function onPointerDown(e: PointerEvent): void {
    const el = deps.scroller();
    if (!el) return;
    if (e.button === 1) {
      // Middle-click autoscroll: always intent, gutter or not.
      escape();
      return;
    }
    if (e.button !== 0 || e.isPrimary === false) return;
    const gutter = el.offsetWidth - el.clientWidth;
    if (gutter <= 0) return;
    const rect = el.getBoundingClientRect();
    const rtl = window.getComputedStyle?.(el).direction === 'rtl';
    const inGutter = rtl ? e.clientX <= rect.left + gutter : e.clientX >= rect.right - gutter;
    if (inGutter) escape();
  }

  /**
   * Scroll events are the one input that never escapes: a programmatic
   * write and a finger are indistinguishable here. Geometry is never
   * intent — it only tells the chip where the target ended up.
   *
   * Coalesced onto a frame: a trackpad flick delivers scroll events far
   * faster than the compositor paints, and every one of them would
   * otherwise cost the chip's three `getBoundingClientRect` reads. The
   * chip answers a question about the CURRENT frame, so measuring once
   * per frame is the whole answer — and the glide's own writes raise
   * scroll events too, which is precisely the burst worth collapsing.
   */
  function onScroll(): void {
    if (scrollFrame !== null) return;
    scrollFrame = requestAnimationFrame(() => {
      scrollFrame = null;
      refreshChip();
    });
  }

  function cancelScrollFrame(): void {
    if (scrollFrame === null) return;
    cancelAnimationFrame(scrollFrame);
    scrollFrame = null;
  }

  // ===== Resize (§9.12) =====

  function runResize(): void {
    resizePending = false;
    refreshChip();
    // While disengaged this writes nothing, deliberately: the reader owns
    // the viewport, and a reflow we did not cause is the browser's to
    // absorb. Re-anchoring it would be a write with no cause they can name.
    followBandCheck();
    // End-to-start spacing: the cooldown starts after the work, so live
    // dragging can never queue recomputation faster than it completes.
    resizeCooldown = setTimeout(() => {
      resizeCooldown = null;
      if (resizePending) runResize();
    }, RESIZE_MIN_INTERVAL_MS);
  }

  function onResize(): void {
    if (resizeCooldown !== null) {
      resizePending = true;
      return;
    }
    runResize();
  }

  // ===== Public surface =====

  function engage(): void {
    if (installFailed) return;
    engaged = true;
    const el = deps.scroller();
    const target = deps.followTargetEl();
    // Unconditional, unlike follow: the reader asked to be taken there,
    // so band membership does not get a vote.
    if (el && target) startGlide(el, restingScrollTop(el, target), 'jump');
    refreshChip();
  }

  function placeOnOpen(): void {
    if (installFailed) return;
    // Open-time state decides the visit; a run that starts running and
    // then parks keeps the follow it opened with until the reader says
    // otherwise.
    engaged = deps.followDefault();
    cancelGlide();
    const el = deps.scroller();
    if (el) {
      const target = deps.followTargetEl();
      // Placement, not scrolling: instant regardless of motion settings,
      // because there is no "before" for the reader to have seen.
      writeScrollTop(engaged && target ? restingScrollTop(el, target) : 0, 'place');
    }
    refreshChip();
  }

  function onFollowTargetChanged(): void {
    // Whatever the band check decides next, a glide already in flight is aimed
    // at where the PREVIOUS target sat, and that destination no longer
    // describes anything. Cancelling it FIRST — including on the branch where
    // the new target needs no move at all — is §9.6's "retargets, never
    // queues" applied to the case the clause does not spell out: letting the
    // old frames keep easing carries the new target straight back out of the
    // band the check just approved.
    cancelGlide();
    followBandCheck();
    refreshChip();
  }

  /**
   * The anchor is the element straddling (or first below) the viewport
   * top — see `pickAnchor` — whose top edge must not move while the map
   * mutates its own layout. Measured here, restored by the returned
   * function once the layout change has landed.
   *
   * While ENGAGED this compensates nothing: follow owns the viewport
   * (§9.7 is scoped to "while not following"), and holding an anchor
   * against a glide would be two writers fighting over one number.
   *
   * An anchor that left the DOM cannot be restored to anything — the
   * element whose top edge was the reference no longer has one — so the
   * hold is dropped rather than compensated against a stand-in.
   */
  function captureAnchor(): () => void {
    const el = deps.scroller();
    if (!el || engaged || installFailed) return noop;
    const anchor = pickAnchor(el);
    if (!anchor) return noop;
    const before = anchor.getBoundingClientRect().top;
    const generation = lifecycle;
    let released = false;
    return () => {
      // Single-shot: a release is one statement about one delta, and a second
      // call would compensate whatever happened to move since, with a cause no
      // reader could name (§9.1).
      if (released) return;
      released = true;
      // The world the measurement described has to still be the one we write
      // into. Follow engaging means the viewport has a different owner; a
      // different scroller or a newer installation means the measurement
      // belongs to a layout nobody is looking at.
      if (engaged || lifecycle !== generation || deps.scroller() !== el) return;
      if (!anchor.isConnected) return;
      const delta = anchor.getBoundingClientRect().top - before;
      if (Number.isFinite(delta) && Math.abs(delta) >= MIN_ANCHOR_DELTA_PX) {
        writeScrollTop(el.scrollTop + delta, 'compensate');
      }
      refreshChip();
    };
  }

  /**
   * The synchronous form: measure, mutate, restore, all before paint.
   * `flushSync` is what makes it synchronous — Svelte state writes flush on
   * their own schedule, and a restore computed before that flush would
   * compensate a delta that has not happened yet.
   */
  function holdAnchor<T>(mutate: () => T): T {
    const release = captureAnchor();
    const result = flushSync(mutate);
    // An async mutation lands its layout change after the flush, so the
    // compensation would be computed against a layout that has not moved
    // yet — a silently wrong scroll position. Loud beats subtle: split the
    // async work out and call holdAnchor around the state flip.
    if (typeof (result as { then?: unknown } | null)?.then === 'function') {
      throw new Error('runMapFollow.holdAnchor: mutate must be synchronous');
    }
    release();
    return result;
  }

  function detach(): void {
    lifecycle += 1;
    const el = attachedEl;
    attachedEl = null;
    if (el) {
      el.removeEventListener('wheel', onWheel);
      el.removeEventListener('keydown', onKeydown);
      el.removeEventListener('touchstart', onTouchStart);
      el.removeEventListener('touchmove', onTouchMove);
      el.removeEventListener('touchend', onTouchEnd);
      el.removeEventListener('touchcancel', onTouchEnd);
      el.removeEventListener('pointerdown', onPointerDown);
      el.removeEventListener('scroll', onScroll);
    }
    if (attachFrame !== null) {
      cancelAnimationFrame(attachFrame);
      attachFrame = null;
    }
    resizeObserver?.disconnect();
    resizeObserver = null;
    if (resizeCooldown !== null) {
      clearTimeout(resizeCooldown);
      resizeCooldown = null;
    }
    resizePending = false;
    touchY = null;
    cancelGlide();
    cancelScrollFrame();
  }

  function install(el: HTMLElement): void {
    // A live installation is the recovery: the listeners exist again, so the
    // reader can escape again, so the chokepoint may write again.
    installFailed = false;
    attachedEl = el;
    // Every listener is passive: this controller never calls
    // preventDefault. It observes intent, it does not veto gestures.
    el.addEventListener('wheel', onWheel, { passive: true });
    el.addEventListener('keydown', onKeydown, { passive: true });
    el.addEventListener('touchstart', onTouchStart, { passive: true });
    el.addEventListener('touchmove', onTouchMove, { passive: true });
    el.addEventListener('touchend', onTouchEnd, { passive: true });
    el.addEventListener('touchcancel', onTouchEnd, { passive: true });
    el.addEventListener('pointerdown', onPointerDown, { passive: true });
    el.addEventListener('scroll', onScroll, { passive: true });
    if (typeof ResizeObserver !== 'undefined') {
      resizeObserver = new ResizeObserver(onResize);
      resizeObserver.observe(el);
    }
    refreshChip();
  }

  function attach(): () => void {
    detach();
    const generation = lifecycle;
    let framesSpent = 0;
    const tryInstall = (): void => {
      attachFrame = null;
      // A teardown or a newer attach retired this attempt; the frame it was
      // riding is not cancelled from inside itself.
      if (lifecycle !== generation) return;
      const el = deps.scroller();
      if (el) {
        install(el);
        return;
      }
      if (framesSpent >= ATTACH_MAX_FRAMES) {
        // Latch BEFORE throwing: the throw is the report, and this is the
        // thing it reports being prevented. Ordering matters — the throw
        // unwinds out of a rAF callback, so nothing after it runs.
        installFailed = true;
        engaged = false;
        chipVisible = false;
        cancelGlide();
        throw new Error(
          'runMapFollow.attach: the overlay scroller never rendered, so the map has '
            + 'no element to listen on — follow would run with no way for the reader to '
            + 'escape it (RUN-MAP §9.2). Check that the frame owning the scroll '
            + 'container binds its element (RUN-MAP §9.9).',
        );
      }
      framesSpent += 1;
      attachFrame = requestAnimationFrame(tryInstall);
    };
    tryInstall();
    // Generation-guarded: a stale cleanup running after a newer attach would
    // otherwise tear the NEWER installation down and leave the controller
    // listening to nothing.
    return () => {
      if (lifecycle === generation) detach();
    };
  }

  return {
    get engaged() {
      return engaged;
    },
    get chipVisible() {
      return chipVisible;
    },
    engage,
    placeOnOpen,
    onFollowTargetChanged,
    holdAnchor,
    captureAnchor,
    attach,
  };
}
