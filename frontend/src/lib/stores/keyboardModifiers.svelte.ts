// Tracks the user's platform modifier-key state with a visibility delay so
// the sidebar can fade in keyboard-jump hint pills only after a deliberate
// hold (mirrors forge / t3-code: a quick modifier tap shouldn't flash hint
// pills on every project row).
//
// Single window listener pair, attached lazily on first subscription
// and cleaned up when the last subscriber disconnects. We deliberately
// don't track other modifiers (shift / alt) — the sidebar only needs the
// platform modifier signal for the thread-jump commands.

import { isMacPlatform } from '../utils/platform';

const HINT_SHOW_DELAY_MS = 100;
const MAX_JUMP_INDEX = 9;

let jumpHintsVisible: boolean = $state(false);
let jumpLabelsByThreadId: ReadonlyMap<string, string> = $state(new Map());
let listenerCount = 0;
let installed = false;
let pendingTimer: ReturnType<typeof setTimeout> | null = null;
let platformIsMacForTest: boolean | null = null;

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
  if (typeof document === 'undefined') {
    jumpLabelsByThreadId = new Map();
    return;
  }
  const next = new Map<string, string>();
  const rows = document.querySelectorAll<HTMLElement>('[data-sidebar-thread-id]');
  for (const row of rows) {
    if (next.size >= MAX_JUMP_INDEX) break;
    const id = row.dataset.sidebarThreadId;
    if (!id || next.has(id)) continue;
    next.set(id, String(next.size + 1));
  }
  jumpLabelsByThreadId = next;
}

function handleKeyDown(event: KeyboardEvent): void {
  if (!isJumpModifier(event)) return;
  if (jumpHintsVisible || pendingTimer) return;
  pendingTimer = setTimeout(() => {
    pendingTimer = null;
    rebuildJumpLabels();
    jumpHintsVisible = true;
  }, HINT_SHOW_DELAY_MS);
}

function handleKeyUp(event: KeyboardEvent): void {
  if (!isJumpModifier(event)) return;
  clearPendingTimer();
  if (jumpHintsVisible) {
    jumpHintsVisible = false;
    jumpLabelsByThreadId = new Map();
  }
}

function handleBlur(): void {
  // Window loses focus mid-hold (cmd-tab to another app) — clear so
  // the hints don't persist when the user comes back without the
  // modifier still down.
  clearPendingTimer();
  if (jumpHintsVisible) {
    jumpHintsVisible = false;
    jumpLabelsByThreadId = new Map();
  }
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
  if (!installed) return;
  if (typeof window === 'undefined') return;
  window.removeEventListener('keydown', handleKeyDown);
  window.removeEventListener('keyup', handleKeyUp);
  window.removeEventListener('blur', handleBlur);
  clearPendingTimer();
  jumpHintsVisible = false;
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
  return () => {
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
 * one of the first 9 visible rows. Reactive — updates when modifier
 * presses re-scan the DOM.
 */
export function jumpLabelForThread(threadId: string): string | undefined {
  return jumpLabelsByThreadId.get(threadId);
}

/** Test helper. */
export function resetKeyboardModifiersForTest(): void {
  clearPendingTimer();
  jumpHintsVisible = false;
  jumpLabelsByThreadId = new Map();
  platformIsMacForTest = null;
  listenerCount = 0;
  if (installed) teardown();
}

export function setKeyboardModifierPlatformForTest(isMac: boolean | null): void {
  platformIsMacForTest = isMac;
}
