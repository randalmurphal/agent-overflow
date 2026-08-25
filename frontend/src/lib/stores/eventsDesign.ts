// Design-mode preview/options event domain: throttled `design:reload-main`
// cache-bust dispatch for the preview iframe and `design:options-update`
// hydration for the N-up options grid. Fan-in target of events.ts's
// setupEventListeners.
import {
  DESIGN_RELOAD_MAIN_EVENT,
} from './eventNames';
import { iterPanes } from './panes.svelte';
import type { ThreadPaneIngest } from './threadPaneRoles';

// The registry hands out whole ThreadPanes; this module narrows them to
// the ingest surface at the one acquisition point, so a new pane member
// use here fails to compile until threadPaneRoles.ts lists it.
function ingestPanes(): Iterable<ThreadPaneIngest> {
  return iterPanes();
}

/**
 * Min interval between consecutive `design:reload-main` cache-bust
 * fires per thread. Watcher events on a hot save loop can land in
 * tight bursts; throttling keeps the iframe from re-creating its
 * document tree more than twice a second.
 */
const DESIGN_RELOAD_THROTTLE_MS = 500;
const designReloadLastFireAt: Map<string, number> = new Map();
const designReloadPending: Map<string, ReturnType<typeof setTimeout>> = new Map();

/**
 * Cap on the per-thread last-fire maps. Entries are only meaningful
 * within their throttle window (≤500ms), but without a cap the maps
 * would accrete one timestamp per design thread ever seen for the
 * session's lifetime. Insertion order approximates recency; evicting
 * the oldest entry at worst lets one reload through un-throttled.
 */
const THROTTLE_MAP_CAP = 64;

function capThrottleMap(map: Map<string, number>): void {
  if (map.size <= THROTTLE_MAP_CAP) return;
  const oldest = map.keys().next();
  if (!oldest.done) map.delete(oldest.value);
}

export interface DesignReloadMainPayload {
  threadId: string;
}
export interface DesignOptionsUpdatePayload {
  threadId: string;
  setId: string;
}

function dispatchDomEvent(name: string, detail: unknown): void {
  if (typeof window === 'undefined') return;
  window.dispatchEvent(new CustomEvent(name, { detail }));
}

function fireReloadMain(threadId: string): void {
  designReloadLastFireAt.set(threadId, Date.now());
  capThrottleMap(designReloadLastFireAt);
  dispatchDomEvent(DESIGN_RELOAD_MAIN_EVENT, { threadId });
}

export function handleDesignReloadMain(payload: DesignReloadMainPayload | undefined): void {
  if (!payload?.threadId) return;
  const threadId = payload.threadId;
  const lastFire = designReloadLastFireAt.get(threadId) ?? 0;
  const elapsed = Date.now() - lastFire;
  if (elapsed >= DESIGN_RELOAD_THROTTLE_MS) {
    const pending = designReloadPending.get(threadId);
    if (pending !== undefined) {
      clearTimeout(pending);
      designReloadPending.delete(threadId);
    }
    fireReloadMain(threadId);
    return;
  }
  // A fire is already pending — coalesce with it.
  if (designReloadPending.has(threadId)) return;
  const delay = DESIGN_RELOAD_THROTTLE_MS - elapsed;
  const handle = setTimeout(() => {
    designReloadPending.delete(threadId);
    fireReloadMain(threadId);
  }, delay);
  designReloadPending.set(threadId, handle);
}

// Same throttle pattern, applied to options-update. Without throttling
// the options panel re-fetches once per file written into options/.
const DESIGN_OTHER_THROTTLE_MS = 250;
type DesignThrottleMaps = {
  lastFire: Map<string, number>;
  pending: Map<string, ReturnType<typeof setTimeout>>;
};
const designOptionsThrottle: DesignThrottleMaps = {
  lastFire: new Map(),
  pending: new Map(),
};

function fireThrottled(
  state: DesignThrottleMaps,
  threadId: string,
  intervalMs: number,
  fire: () => void,
): void {
  const lastFire = state.lastFire.get(threadId) ?? 0;
  const elapsed = Date.now() - lastFire;
  if (elapsed >= intervalMs) {
    const pending = state.pending.get(threadId);
    if (pending !== undefined) {
      clearTimeout(pending);
      state.pending.delete(threadId);
    }
    state.lastFire.set(threadId, Date.now());
    capThrottleMap(state.lastFire);
    fire();
    return;
  }
  if (state.pending.has(threadId)) return;
  const delay = intervalMs - elapsed;
  const handle = setTimeout(() => {
    state.pending.delete(threadId);
    state.lastFire.set(threadId, Date.now());
    capThrottleMap(state.lastFire);
    fire();
  }, delay);
  state.pending.set(threadId, handle);
}

function clearDesignThrottle(state: DesignThrottleMaps): void {
  for (const handle of state.pending.values()) {
    clearTimeout(handle);
  }
  state.pending.clear();
  state.lastFire.clear();
}

export function applyDesignOptionsUpdate(payload: DesignOptionsUpdatePayload): void {
  if (!payload?.threadId) return;
  const detail = payload;
  fireThrottled(designOptionsThrottle, payload.threadId, DESIGN_OTHER_THROTTLE_MS, () => {
    for (const pane of ingestPanes()) {
      if (pane.threadId === detail.threadId) {
        void pane.applyDesignOptionsUpdate(detail.threadId, detail.setId ?? '');
      }
    }
  });
}

export function clearAllDesignThrottles(): void {
  // Drop any pending throttled reloads + per-thread last-fire
  // bookkeeping so a re-attached listener starts from a clean state.
  for (const handle of designReloadPending.values()) {
    clearTimeout(handle);
  }
  designReloadPending.clear();
  designReloadLastFireAt.clear();
  clearDesignThrottle(designOptionsThrottle);
}
