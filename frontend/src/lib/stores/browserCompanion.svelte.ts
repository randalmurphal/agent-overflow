import {
  BrowserCompanionResize,
  BrowserCompanionNextFrame,
  BrowserCompanionSubscribe,
  BrowserCompanionUnsubscribe,
  type BrowserCompanionEvent,
} from './bindings';
import { createEntityStore, type EntityAttachment } from './entityStore.svelte';
import { closeCompanion, companionForSource, openCompanion } from './companionPanes.svelte';
import { addPaneThreadMountedObserver, getAllPanes } from './panes.svelte';

export interface BrowserCompanionView {
  state: BrowserCompanionEvent;
  frame: string;
  framePageId: string;
  frameWidth: number;
  frameHeight: number;
  frameSequence: number;
  error: string;
}

interface BrowserCompanionCtx {
  width: number;
  height: number;
}

const subscriptionIds = new Map<string, string>();
const liveStates = new Map<string, BrowserCompanionEvent>();

function emptyView(state: BrowserCompanionEvent): BrowserCompanionView {
  return {
    state,
    frame: '',
    framePageId: '',
    frameWidth: 0,
    frameHeight: 0,
    frameSequence: 0,
    error: '',
  };
}

const store = createEntityStore<BrowserCompanionView, BrowserCompanionCtx>({
  name: 'browserCompanion',
  rawValue: true,
  source: async ({ key, getCtx, apply }) => {
    const ctx = getCtx();
    const result = await BrowserCompanionSubscribe(key, ctx.width, ctx.height);
    subscriptionIds.set(key, result.id);
    apply(emptyView(result.state));
    let active = true;
    void (async () => {
      while (active) {
        try {
          const frame = await BrowserCompanionNextFrame(result.id);
          if (active) applyBrowserCompanionFrame(frame);
        } catch (err) {
          if (!active) return;
          const previous = store.snapshot(key);
          if (previous) store.apply(key, { ...previous, error: String(err) });
          return;
        }
      }
    })();
    return async () => {
      active = false;
      if (subscriptionIds.get(key) === result.id) subscriptionIds.delete(key);
      await BrowserCompanionUnsubscribe(result.id);
    };
  },
});

export function attachBrowserCompanion(
  threadId: string,
  width: number,
  height: number,
): EntityAttachment<BrowserCompanionView> {
  return store.attach(threadId, { width, height });
}

export async function resizeBrowserCompanion(threadId: string, width: number, height: number): Promise<void> {
  const id = subscriptionIds.get(threadId);
  if (id) await BrowserCompanionResize(id, width, height);
}

function sourcePaneIdForThread(threadId: string): string | null {
  for (const [paneId, pane] of getAllPanes()) {
    if (pane.threadId === threadId) return paneId;
  }
  return null;
}

export function applyBrowserCompanionState(event: BrowserCompanionEvent): void {
  if (!event?.threadId) return;
  if (event.kind === 'error') {
    const previous = store.snapshot(event.threadId);
    if (previous) store.apply(event.threadId, { ...previous, error: event.error || 'Browser stream failed' });
    return;
  }
  if (event.kind !== 'state') return;
  if ((event.pages?.length ?? 0) > 0) liveStates.set(event.threadId, event);
  else liveStates.delete(event.threadId);
  const sourcePaneId = sourcePaneIdForThread(event.threadId);
  if (sourcePaneId) {
    const existing = companionForSource(sourcePaneId, 'browser');
    if ((event.pages?.length ?? 0) > 0 && event.visible === true) {
      if (!existing) openCompanion(sourcePaneId, 'browser');
    } else if (existing) {
      closeCompanion(existing.paneId);
    }
  }
  const previous = store.snapshot(event.threadId);
  store.apply(
    event.threadId,
    previous
      ? {
          ...previous,
          state: event,
          frame: previous.state.activePageId === event.activePageId ? previous.frame : '',
          framePageId: previous.state.activePageId === event.activePageId ? previous.framePageId : '',
          error: '',
        }
      : emptyView(event),
  );
}

export function reconcileBrowserCompanionForPane(paneId: string, threadId: string): void {
  if (
    liveStates.get(threadId)?.visible === true &&
    (liveStates.get(threadId)?.pages?.length ?? 0) > 0 &&
    !companionForSource(paneId, 'browser')
  ) {
    openCompanion(paneId, 'browser');
  }
}

addPaneThreadMountedObserver(reconcileBrowserCompanionForPane);

export function applyBrowserCompanionFrame(event: BrowserCompanionEvent): void {
  if (!event?.threadId || event.kind !== 'frame' || !event.frame) return;
  const previous = store.snapshot(event.threadId);
  if (!previous || event.pageId !== previous.state.activePageId) return;
  store.apply(event.threadId, {
    ...previous,
    frame: event.frame,
    framePageId: event.pageId ?? '',
    frameWidth: event.width ?? 0,
    frameHeight: event.height ?? 0,
    frameSequence: event.sequence ?? 0,
  });
}

export function resetBrowserCompanionForTest(): void {
  subscriptionIds.clear();
  liveStates.clear();
  store.suspend();
  store.resetAll();
}
