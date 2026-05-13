// Persisted layout state for the design-mode split. The chat column and
// preview iframe are separately resizable via DesignSplitResizer; this
// store holds the chat column's width in pixels (preview width follows
// from container width − chat − resizer width). One slot is enough
// because v1 runs a single main pane; if pane tiling lands later, this
// becomes per-pane state.

const STORAGE_KEY = 'agent-overflow:designLayout:chatPx';
const STORAGE_KEY_PREFIX = 'agent-overflow:designLayout:chatPx:';

// Min sizes are spec-defined: chat ≥ 320px, preview ≥ 400px. The default
// fraction (chat = 45% of container) is applied in computeChatWidth() the
// first time we have a container measurement; we don't persist a sentinel
// for "use default" because we want any user-driven resize to win
// permanently after first interaction.
export const DESIGN_CHAT_MIN_PX = 320;
export const DESIGN_PREVIEW_MIN_PX = 400;
export const DESIGN_CHAT_DEFAULT_FRACTION = 0.45;

function storageKeyForPane(paneId = 'main'): string {
  return paneId === 'main' ? STORAGE_KEY : `${STORAGE_KEY_PREFIX}${paneId}`;
}

function readPersistedPx(paneId = 'main'): number | null {
  if (typeof localStorage === 'undefined') return null;
  try {
    const raw = localStorage.getItem(storageKeyForPane(paneId));
    if (!raw) return null;
    const value = Number(raw);
    if (!Number.isFinite(value) || value <= 0) return null;
    return value;
  } catch {
    return null;
  }
}

function writePersistedPx(value: number, paneId = 'main'): void {
  if (typeof localStorage === 'undefined') return;
  try {
    localStorage.setItem(storageKeyForPane(paneId), String(Math.round(value)));
  } catch {
    // ignore — storage may be disabled (private mode) or quota-full.
  }
}

let chatPxByPane: Map<string, number | null> = $state(new Map([['main', readPersistedPx('main')]]));

function getPaneChatPx(paneId = 'main'): number | null {
  if (chatPxByPane.has(paneId)) return chatPxByPane.get(paneId) ?? null;
  const value = readPersistedPx(paneId);
  chatPxByPane = new Map(chatPxByPane).set(paneId, value);
  return value;
}

function peekPaneChatPx(paneId = 'main'): number | null {
  if (chatPxByPane.has(paneId)) return chatPxByPane.get(paneId) ?? null;
  return readPersistedPx(paneId);
}

/**
 * Resolve the chat column's width in pixels for a given container width.
 * Falls back to DESIGN_CHAT_DEFAULT_FRACTION when no persisted value
 * exists. Clamps so neither pane drops below its minimum size.
 */
export function computeChatWidth(containerPx: number, paneId = 'main'): number {
  const fallback = Math.round(containerPx * DESIGN_CHAT_DEFAULT_FRACTION);
  const candidate = peekPaneChatPx(paneId) ?? fallback;
  return clampChatWidth(candidate, containerPx);
}

/**
 * Clamp a candidate chat-pane width to the container, respecting both
 * panes' minimums. Used by the resizer during pointermove to keep the
 * preview pane from collapsing below its floor.
 */
export function clampChatWidth(candidate: number, containerPx: number): number {
  if (containerPx <= 0) return DESIGN_CHAT_MIN_PX;
  // Preserve the resizer's own width budget — it lives between the panes
  // and visually subtracts a few pixels from the available area.
  const RESIZER_PX = 4;
  const upper = Math.max(
    DESIGN_CHAT_MIN_PX,
    containerPx - DESIGN_PREVIEW_MIN_PX - RESIZER_PX,
  );
  if (candidate < DESIGN_CHAT_MIN_PX) return DESIGN_CHAT_MIN_PX;
  if (candidate > upper) return upper;
  return candidate;
}

export function getChatPx(paneId = 'main'): number | null {
  return getPaneChatPx(paneId);
}

export function setChatPx(next: number, paneId = 'main'): void {
  chatPxByPane = new Map(chatPxByPane).set(paneId, next);
}

export function persistChatPx(paneId = 'main'): void {
  const chatPx = getPaneChatPx(paneId);
  if (chatPx !== null) writePersistedPx(chatPx, paneId);
}
