// The latest focus intent: a focus the app applies later than the choice
// that asked for it (a terminal once its surface mounts, a browser page once
// the native host applies it). Any later focus choice cancels it: a newer
// intent, a focus the app moves at once (focusChosen, a keyboard pane move's
// carry), and a focus or press outside the intent's scope, the user's or a
// surface's own (a dialog's input). Where document.activeElement is when the
// deferred focus lands cannot tell a choice made meanwhile from one made
// before; the intent's identity can.
//
// A modal giving focus back to what held it before it opened
// (returnFocus) is no new choice.

/** Where focus and presses belong to the intent rather than to another choice. */
export interface FocusScope {
  /** A focus landing here is on the intent's own path (its pane's mount focus). */
  readonly holdsFocus: (node: Node) => boolean;
  /** A press here is on the surface the intent will focus. */
  readonly holdsPress: (node: Node) => boolean;
}

/** One deferred focus. Ended by landing, by its owner, or by a later choice. */
export interface FocusIntent {
  /** Whether no later focus choice was made and it has not ended. */
  readonly current: boolean;
  /** Runs `focus` while the intent is current, and ends it. Whether it ran. */
  land(focus: () => void): boolean;
  /** Ends it without focusing and without its cancel callback. */
  end(): void;
}

interface Entry {
  readonly scope: FocusScope;
  readonly onCanceled: () => void;
  ended: boolean;
}

let latest: Entry | null = null;
let returning = 0;

/**
 * Registers a focus to apply later, within `scope`. It replaces the latest
 * intent, which is canceled. `onCanceled` runs once when a later choice
 * cancels this one, not when it lands or its owner ends it.
 */
export function intendFocus(scope: FocusScope, onCanceled: () => void = () => {}): FocusIntent {
  cancelLatest();
  const entry: Entry = { scope, onCanceled, ended: false };
  latest = entry;
  listen();
  return {
    get current() {
      return !entry.ended;
    },
    land(focus) {
      if (entry.ended) return false;
      finish(entry);
      focus();
      return true;
    },
    end() {
      finish(entry);
    },
  };
}

/** A focus the app moves now: no deferred one lands after it. */
export function focusChosen(): void {
  cancelLatest();
}

/** Runs `focus`, which gives focus back to what held it before a modal opened; it cancels nothing. */
export function returnFocus(focus: () => void): void {
  returning += 1;
  try {
    focus();
  } finally {
    returning -= 1;
  }
}

/** A scope of the pane `paneId`'s section, its own surface being where `surface` matches. */
export function paneFocusScope(paneId: string, surface: string): FocusScope {
  const inPane = (node: Node) => {
    const section = elementOf(node)?.closest('[data-pane-id]');
    return section instanceof HTMLElement && section.dataset.paneId === paneId;
  };
  return {
    holdsFocus: inPane,
    holdsPress: (node) => inPane(node) && (elementOf(node)?.closest(surface) ?? null) !== null,
  };
}

function elementOf(node: Node): Element | null {
  return node instanceof Element ? node : node.parentElement;
}

/** A scope of one element, the surface the intent focuses. */
export function elementFocusScope(element: Element): FocusScope {
  const within = (node: Node) => element.contains(node);
  return { holdsFocus: within, holdsPress: within };
}

function finish(entry: Entry): void {
  if (entry.ended) return;
  entry.ended = true;
  if (latest === entry) {
    latest = null;
    unlisten();
  }
}

function cancelLatest(): void {
  const entry = latest;
  if (!entry) return;
  finish(entry);
  entry.onCanceled();
}

function onFocusIn(event: FocusEvent): void {
  if (returning > 0 || !latest) return;
  const target = event.target;
  if (target instanceof Node && latest.scope.holdsFocus(target)) return;
  cancelLatest();
}

function onPointerDown(event: Event): void {
  if (!latest) return;
  const target = event.target;
  if (target instanceof Node && latest.scope.holdsPress(target)) return;
  cancelLatest();
}

function listen(): void {
  if (typeof document === 'undefined') return;
  document.addEventListener('focusin', onFocusIn, true);
  window.addEventListener('pointerdown', onPointerDown, true);
}

function unlisten(): void {
  if (typeof document === 'undefined') return;
  document.removeEventListener('focusin', onFocusIn, true);
  window.removeEventListener('pointerdown', onPointerDown, true);
}

/** Test hook: ends the latest intent without its callback. */
export function resetFocusIntentForTest(): void {
  if (latest) finish(latest);
  returning = 0;
}
