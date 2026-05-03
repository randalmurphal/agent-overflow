// Shared core for the Discussion-mode `stickToBottom.svelte.ts` plain-DOM
// scroll controller (ChannelView). The chat surface used to share this
// too via `stickyBottomController`, but the spring controller in
// `useStickToBottom.svelte.ts` has its own gesture model — the two
// surfaces are now legitimately distinct.
//
// Owns the user-intent state machine — what it means to be `'stick'` vs
// `'free'`, when wheel/keyboard/touch gestures flip between them, the
// down-gesture window for restick — plus the pause-lease semantics. The
// surface-specific controller stays a thin wrapper that supplies geometry
// (scroll offset / size / viewport) and the actual scroll-write call,
// then calls into this core for everything else.
//
// Why this still lives separately: keeping the gesture/intent state
// machine in one file makes future Discussion-mode changes (e.g. "what
// counts as a down-gesture" or "how long the restick window is") a
// one-line edit.

export type StickIntent = 'stick' | 'free';

const UP_KEYS: ReadonlySet<string> = new Set(['PageUp', 'ArrowUp', 'Home']);
const DOWN_KEYS: ReadonlySet<string> = new Set(['PageDown', 'ArrowDown', 'End']);

const DEFAULT_GESTURE_WINDOW_MS = 250;

function nowMs(): number {
  return typeof performance !== 'undefined' ? performance.now() : Date.now();
}

export interface ScrollIntentCoreOptions {
  /**
   * Window after a down-gesture during which the next scroll event can
   * confirm restick. Default 250ms.
   */
  gestureWindowMs?: number;
}

export interface ScrollIntentCore {
  /** Reactive intent state. Read in $derived / $effect / templates. */
  readonly intent: StickIntent;
  /** Reactive shorthand for `intent === 'stick'`. */
  readonly isSticky: boolean;
  /** Mutate intent. No-op when value is unchanged. */
  setIntent(next: StickIntent): void;

  /** Record a "user moved DOWN" gesture for the restick window. */
  noteDownGesture(): void;
  /** True iff a down-gesture occurred within the gesture window. */
  inDownGestureWindow(): boolean;
  /** Reset the down-gesture timestamp (call after a successful restick). */
  clearDownGesture(): void;

  /** Pointer-down flag — surface-specific controllers update this from their pointer handlers. */
  isPointerDown(): boolean;
  setPointerDown(next: boolean): void;

  /** True iff a pause-lease is active. */
  isPaused(): boolean;
  /** Depth-counted lease that suspends auto-scroll until released. Idempotent dispose. */
  pauseAutoScroll(): () => void;

  /**
   * `intent === 'stick' && !pointerDown && !paused`. The condition every
   * auto-follow path checks before calling its scroll-write.
   */
  canAutoScroll(): boolean;

  /**
   * Bind wheel/keyboard/touch listeners on `el` that flip intent or note
   * down-gestures. Pointer events are NOT bound here because pointerup
   * needs surface-specific geometry (DOM scrollTop vs VList offset) to
   * decide whether the drag scrolled up or down. Returns a detacher.
   */
  bindGestureListeners(el: HTMLElement): () => void;

  /** Reset transient gesture state so a future attach starts clean. */
  resetTransientState(): void;
}

export function createScrollIntentCore(
  options: ScrollIntentCoreOptions = {},
): ScrollIntentCore {
  const gestureWindowMs = options.gestureWindowMs ?? DEFAULT_GESTURE_WINDOW_MS;

  let intent = $state<StickIntent>('stick');
  let pointerDown = false;
  let lastDownGestureAt = 0;
  let touchStartY: number | null = null;
  let pauseDepth = 0;

  function setIntent(next: StickIntent): void {
    if (intent !== next) intent = next;
  }

  function noteDownGesture(): void {
    lastDownGestureAt = nowMs();
  }

  function inDownGestureWindow(): boolean {
    if (lastDownGestureAt === 0) return false;
    return nowMs() - lastDownGestureAt < gestureWindowMs;
  }

  function clearDownGesture(): void {
    lastDownGestureAt = 0;
  }

  function isPointerDown(): boolean {
    return pointerDown;
  }

  function setPointerDown(next: boolean): void {
    pointerDown = next;
  }

  function isPaused(): boolean {
    return pauseDepth > 0;
  }

  function pauseAutoScroll(): () => void {
    pauseDepth += 1;
    let released = false;
    return () => {
      if (released) return;
      released = true;
      pauseDepth = Math.max(0, pauseDepth - 1);
    };
  }

  function canAutoScroll(): boolean {
    return intent === 'stick' && !pointerDown && pauseDepth === 0;
  }

  // ===== Gesture handlers =====

  function handleWheel(e: WheelEvent): void {
    if (e.deltaY < 0) {
      setIntent('free');
    } else if (e.deltaY > 0) {
      noteDownGesture();
    }
  }

  function handleKeydown(e: KeyboardEvent): void {
    if (UP_KEYS.has(e.key)) {
      setIntent('free');
    } else if (DOWN_KEYS.has(e.key)) {
      noteDownGesture();
    }
  }

  function handleTouchStart(e: TouchEvent): void {
    touchStartY = e.touches[0]?.clientY ?? null;
  }

  function handleTouchMove(e: TouchEvent): void {
    if (touchStartY === null) return;
    const y = e.touches[0]?.clientY ?? touchStartY;
    const dy = y - touchStartY;
    touchStartY = y;
    // Finger moves DOWN (dy > 0) → content moves DOWN visually → user
    // wants to see content ABOVE → flip free. Finger moves UP → user is
    // scrolling content up → mark down-gesture for restick window.
    if (dy > 1) {
      setIntent('free');
    } else if (dy < -1) {
      noteDownGesture();
    }
  }

  function handleTouchEnd(): void {
    touchStartY = null;
  }

  function bindGestureListeners(el: HTMLElement): () => void {
    el.addEventListener('wheel', handleWheel, { passive: true });
    el.addEventListener('keydown', handleKeydown);
    el.addEventListener('touchstart', handleTouchStart, { passive: true });
    el.addEventListener('touchmove', handleTouchMove, { passive: true });
    el.addEventListener('touchend', handleTouchEnd, { passive: true });
    el.addEventListener('touchcancel', handleTouchEnd, { passive: true });
    return () => {
      el.removeEventListener('wheel', handleWheel);
      el.removeEventListener('keydown', handleKeydown);
      el.removeEventListener('touchstart', handleTouchStart);
      el.removeEventListener('touchmove', handleTouchMove);
      el.removeEventListener('touchend', handleTouchEnd);
      el.removeEventListener('touchcancel', handleTouchEnd);
    };
  }

  function resetTransientState(): void {
    pointerDown = false;
    lastDownGestureAt = 0;
    touchStartY = null;
  }

  return {
    get intent() {
      return intent;
    },
    get isSticky() {
      return intent === 'stick';
    },
    setIntent,
    noteDownGesture,
    inDownGestureWindow,
    clearDownGesture,
    isPointerDown,
    setPointerDown,
    isPaused,
    pauseAutoScroll,
    canAutoScroll,
    bindGestureListeners,
    resetTransientState,
  };
}
