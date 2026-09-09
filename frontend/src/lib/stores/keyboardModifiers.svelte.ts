// Tracks the user's platform modifier-key state with a visibility delay so
// the sidebar can fade in keyboard-jump hint pills only after a deliberate
// hold (a quick modifier tap shouldn't flash hint
// pills on every project row).
//
// Single window listener pair, attached lazily on first subscription
// and cleaned up when the last subscriber disconnects. We deliberately
// don't track other modifiers (shift / alt) — the sidebar only needs the
// platform modifier signal for the thread-jump commands.

import { isMacPlatform } from '../utils/platform';
import { getSidebarJumpThreadIds } from './sidebarThreadOrder';

const HINT_SHOW_DELAY_MS = 100;

let jumpHintsVisible: boolean = $state(false);
let jumpLabelsByThreadId: ReadonlyMap<string, string> = $state(new Map());
let listenerCount = 0;
let installed = false;
let pendingTimer: ReturnType<typeof setTimeout> | null = null;
let platformIsMacForTest: boolean | null = null;
const rowObservers = new Map<HTMLElement, MutationObserver>();

function isJumpModifier(event: KeyboardEvent): boolean {
  const isMac = platformIsMacForTest ?? isMacPlatform();
  return isMac ? event.key === 'Meta' : event.key === 'Control';
}

function clearPendingTimer(): void {
  if (pendingTimer) {
    clearTimeout(pendingTimer);
    pendingTimer = null;
  }
}

function rebuildJumpLabels(): void {
  const next = new Map<string, string>();
  for (const [index, id] of getSidebarJumpThreadIds().entries()) {
    next.set(id, String(index + 1));
  }
  if (next.size === jumpLabelsByThreadId.size
    && [...next].every(([id, label]) => jumpLabelsByThreadId.get(id) === label)) return;
  jumpLabelsByThreadId = next;
}

function containsThreadRow(node: Node): boolean {
  if (node.nodeType !== Node.ELEMENT_NODE) return false;
  const element = node as Element;
  return element.matches('[data-sidebar-thread-id]')
    || element.querySelector('[data-sidebar-thread-id]') !== null;
}

function observeRows(root: HTMLElement, observer: MutationObserver): void {
  observer.observe(root, {
    subtree: true,
    childList: true,
    attributes: true,
    attributeFilter: ['data-sidebar-thread-id', 'data-sidebar-jump-target'],
  });
}

/** Svelte action: observe only mounted sidebar trees, and only during a hold. */
export function trackSidebarJumpRows(root: HTMLElement): { destroy: () => void } {
  const observer = new MutationObserver((records) => {
    if (!jumpHintsVisible) return;
    if (records.some((record) => record.type === 'attributes'
      || [...record.addedNodes, ...record.removedNodes].some(containsThreadRow))) {
      rebuildJumpLabels();
    }
  });
  rowObservers.set(root, observer);
  if (jumpHintsVisible) {
    observeRows(root, observer);
    rebuildJumpLabels();
  }
  return {
    destroy: () => {
      observer.disconnect();
      rowObservers.delete(root);
      // An action can be destroyed before its DOM subtree is removed.
      // Clear hints now; surviving roots are rescanned after removal.
      if (jumpHintsVisible) {
        jumpLabelsByThreadId = new Map();
        queueMicrotask(() => { if (jumpHintsVisible) rebuildJumpLabels(); });
      }
    },
  };
}

function hideJumpHints(): void {
  clearPendingTimer();
  for (const observer of rowObservers.values()) observer.disconnect();
  jumpHintsVisible = false;
  jumpLabelsByThreadId = new Map();
}

function handleKeyDown(event: KeyboardEvent): void {
  if (!isJumpModifier(event)) return;
  if (jumpHintsVisible || pendingTimer) return;
  pendingTimer = setTimeout(() => {
    pendingTimer = null;
    rebuildJumpLabels();
    jumpHintsVisible = true;
    for (const [root, observer] of rowObservers) observeRows(root, observer);
  }, HINT_SHOW_DELAY_MS);
}

function handleKeyUp(event: KeyboardEvent): void {
  if (!isJumpModifier(event)) return;
  hideJumpHints();
}

function handleBlur(): void {
  // Window loses focus mid-hold (cmd-tab to another app) — clear so
  // the hints don't persist when the user comes back without the
  // modifier still down.
  hideJumpHints();
}

function ensureInstalled(): void {
  if (installed) return;
  if (typeof window === 'undefined') return;
  window.addEventListener('keydown', handleKeyDown);
  window.addEventListener('keyup', handleKeyUp);
  window.addEventListener('blur', handleBlur);
  installed = true;
}

function teardown(): void {
  hideJumpHints();
  if (!installed) return;
  if (typeof window === 'undefined') return;
  window.removeEventListener('keydown', handleKeyDown);
  window.removeEventListener('keyup', handleKeyUp);
  window.removeEventListener('blur', handleBlur);
  installed = false;
}

/**
 * Subscribe to jump-hint visibility. Increments a refcount so multiple
 * components can mount and unmount independently; the listener tears
 * down when the last subscriber leaves. Returns a function the caller
 * runs in `onDestroy`.
 */
export function subscribeJumpHints(): () => void {
  ensureInstalled();
  listenerCount += 1;
  let released = false;
  return () => {
    if (released) return;
    released = true;
    listenerCount -= 1;
    if (listenerCount <= 0) {
      listenerCount = 0;
      teardown();
    }
  };
}

export function getJumpHintsVisible(): boolean {
  return jumpHintsVisible;
}

/**
 * Look up a row's jump-hint label ("1".."9") or undefined if it isn't
 * one of the first 9 rendered front-burner pin targets.
 */
export function jumpLabelForThread(threadId: string): string | undefined {
  return jumpLabelsByThreadId.get(threadId);
}

/** Test helper. */
export function resetKeyboardModifiersForTest(): void {
  teardown();
  rowObservers.clear();
  platformIsMacForTest = null;
  listenerCount = 0;
}

export function setKeyboardModifierPlatformForTest(isMac: boolean | null): void {
  platformIsMacForTest = isMac;
}
