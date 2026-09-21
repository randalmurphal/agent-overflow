import type { Item, Thread } from '../types/models';
import type { TimelineSelection, TimelineScopeContext } from '../../../bindings/agent-overflow/internal/store/models';
import { createThreadItemWindow } from './threadItemWindow.svelte';
import { createThreadRowUiState } from './threadRowUiState.svelte';
import { createThreadStreamingReveal } from './threadStreamingReveal.svelte';
import { createThreadTimelineWindow } from './threadTimelineWindow.svelte';
import { createThreadActivityRuns } from './threadActivityRuns.svelte';
import { createThreadItemStreamApply } from './threadItemStreamApply';
import { createThreadPaneScroll } from './threadPaneScroll.svelte';
import { activityRunDefaultCollapsed, activityRunWindowRows } from './activityRunPrefs.svelte';
import { timelinePageShape, nowForLiveContent } from './threadPaneShared';
import { attachTimelineWindow, type WindowObservation } from './timelineWindowResource.svelte';
import { registerTimelineSurface, type TimelineMutation } from './timelineSurfaces';
import { readTimelineWindow } from './readTimelineWindow';
import { awaitBackendReplay } from './transportRecovery';
import { threadBackend } from '../transport/entityIndex';
import { threadHasScope } from '../transport/entityScopes';
import { requireEntityBackend } from '../transport/backends';
import { onThreadHistoryInvalidated } from './threadIdentityInvalidation';
import { getThreadScrollSnapshot } from '../utils/threadScrollSnapshots';
import { isWindowedTimelineRow } from './threadWindowDigest';
import { compareItemsByTimelinePosition, isItemStatusRegression } from './threadItems';
import { createRefreshScheduler } from '../utils/refreshScheduler';
import { reportFrontendDiagnostic } from '../utils/frontendErrorCapture';
import { parseJsonObject } from '../utils/parseJsonObject';
import { errString } from '../utils/errors';
import { addToast } from './toast.svelte';

/** A surface owns its reading window, run expansion, payload leases and reveal state. */
export function createScopedTimeline(thread: Thread, selection: TimelineSelection, key: string) {
  if (!selection.scopeRootId) throw new Error('A scoped timeline requires its transcript root');
  let scope = $state.raw<TimelineScopeContext | null>(null);
  let loading = $state(true);
  let gone = $state(false);
  let generation = $state(0);
  let lastLiveContentAt = 0;
  let requestGeneration = 0;
  let contextVersion = 0;
  let missedStreamUpdate = false;
  let disposed = false;
  let attachment = $state.raw<ReturnType<typeof attachTimelineWindow> | null>(null);
  const reads = new Map<AbortSignal, Set<string>>();
  const unheldMutations = new Map<AbortSignal, Set<string>>();
  const optimisticItemIds = new Set<string>();
  const includes = (item: Item) => isWindowedTimelineRow(item, selection);
  const noteItemMutation = (id: string) => { for (const touched of reads.values()) touched.add(id); };
  const mutations = {
    noteItemMutation,
    noteItemMutations(items: readonly Item[]) { for (const item of items) noteItemMutation(item.id); },
    noteItemWindowReplacement(before: readonly Item[], after: readonly Item[]) {
      for (const item of [...before, ...after]) noteItemMutation(item.id);
    },
  };
  const itemWindow = createThreadItemWindow({
    optimisticItemIds, streamingReveal: () => reveal, rowUiState: () => rows,
    activityRuns: () => runs, switchLoad: () => mutations,
  });
  const { getItems, getItemById, itemIndexById, writeItemAt, appendDirectAssistantLiteral,
    replaceTimelineItems, installTimelineItems, commitUpsertResult } = itemWindow;
  const rows = createThreadRowUiState({ getItemById, loadedItems: getItems });
  const scroll = createThreadPaneScroll({
    getThread: () => thread, getLoading: () => loading, getSwitchGeneration: () => generation,
    getItemCount: () => getItems().length, stampLiveContent: () => { lastLiveContentAt = nowForLiveContent(); },
  });
  const reveal = createThreadStreamingReveal({
    get scopeRootId() { return selection.scopeRootId; },
    getItems, getItemById, getItemIndex: id => itemIndexById.get(id), setItemAt: writeItemAt,
    appendDirectAssistantLiteral, stampLiveContent: () => { lastLiveContentAt = nowForLiveContent(); },
    armStructuralSpring: scroll.armLiveContentAppendSpring,
    appendLivePayloadDeltaForItem: rows.appendLivePayloadDeltaForItem,
  });
  const window = createThreadTimelineWindow({
    getItems, replaceTimelineItems, installTimelineItems, getThread: () => thread,
    getSwitchGeneration: () => generation, getScrollController: () => scroll.controller,
    activityRuns: () => runs, selection: () => selection,
  });
  const runs = createThreadActivityRuns({
    defaultCollapsed: activityRunDefaultCollapsed, windowRows: activityRunWindowRows,
    windowVerified: () => !loading, scrollController: () => scroll.controller, items: getItems,
    windowBounds: () => ({ oldest: window.oldestLoadedCursor, newest: window.newestLoadedCursor }),
    threadId: () => disposed ? null : thread.id, selection: () => selection,
    pageShape: timelinePageShape, mountRunMembers: window.mountActivityRunMembers,
    reloadWindow: () => refresh(),
    reportFetchFailure: (message, error, silent) => {
      reportFrontendDiagnostic('scoped timeline member load failed', errString(error));
      if (!silent) addToast('error', message);
    },
  });
  const stream = createThreadItemStreamApply({
    getItems, getItemById, itemIndexById, getThread: () => thread, writeItemAt, commitUpsertResult,
    armLiveContentAppendSpring: scroll.armLiveContentAppendSpring, optimisticItemIds,
    timelineWindow: window, streamingReveal: reveal, activityRuns: runs, get scopeRootId() { return selection.scopeRootId; },
  });
  const contextRefresh = createRefreshScheduler({
    name: 'agent scope context', delayMs: 100, maxWaitMs: 500,
    run: () => refresh().catch(error => reportFrontendDiagnostic('scoped timeline refresh failed', errString(error))),
  });
  function remove(ids: ReadonlySet<string>) {
    if (ids.size) window.invalidatePendingReads();
    for (const id of ids) noteItemMutation(id);
    const next = getItems().filter(item => !ids.has(item.id));
    if (next.length !== getItems().length) replaceTimelineItems(next, { disposeDropped: true });
  }
  function applyMutation(mutation: TimelineMutation) {
    if (gone || disposed) return;
    if (mutation.kind === 'upsert') {
      const accepted: Item[] = [];
      const moved = new Set<string>();
      for (const item of mutation.items) {
        const contextItem = scope && (item.id === scope.root.id ? scope.root
          : item.id === scope.lifecycle.id ? scope.lifecycle
          : item.id === scope.completion?.id ? scope.completion : undefined);
        if (scope && contextItem && !isItemStatusRegression(contextItem as Item, item)) {
          contextVersion++;
          scope = { ...scope,
            root: item.id === scope.root.id ? item : scope.root,
            lifecycle: item.id === scope.lifecycle.id ? item : scope.lifecycle,
            completion: item.id === scope.completion?.id ? item : scope.completion };
        }
        if (includes(item)) accepted.push(item);
        else {
          if (reads.size) noteItemMutation(item.id);
          if (getItemById(item.id)) moved.add(item.id);
        }
        // Before context resolves, a live row may belong to its canonical root.
        if (!scope || item.id === selection.scopeRootId || item.id === scope.lifecycle.id
          || item.completionOf === scope?.lifecycle.id
          || parseJsonObject(item.meta)?.transcript_root_id === selection.scopeRootId) contextRefresh.request();
      }
      remove(moved);
      if (stream.applyProviderItemUpserts(accepted)?.changedItems.length) lastLiveContentAt = nowForLiveContent();
      if (reads.size && accepted.some(item => !getItemById(item.id))) missedStreamUpdate = true;
      return;
    }
    if (mutation.kind === 'remove') {
      remove(new Set([mutation.itemId]));
      if (mutation.itemId === selection.scopeRootId) { gone = true; generation++; }
      else contextRefresh.request();
      return;
    }
    if (mutation.kind === 'revert') {
      const cut = mutation.event;
      const kept = new Set(cut.keptAnchorTurnItemIds ?? []);
      const survives = (item: Item) => item.turnIndex < cut.turnIndex
        || (item.turnIndex === cut.turnIndex && kept.has(item.id));
      remove(new Set(getItems().filter(item => !survives(item)).map(item => item.id)));
      window.applyConversationCut(false);
      if (scope?.root && !survives(scope.root as Item)) { gone = true; generation++; }
      else contextRefresh.request();
      return;
    }
    const event = mutation.event;
    if (scope && [scope.root.id, scope.lifecycle.id, scope.completion?.id].includes(event.itemId)) {
      const update = (item: Item): Item => {
        if (item.id !== event.itemId) return item;
        if (mutation.kind === 'meta') return { ...item, meta: mutation.event.meta };
        if (mutation.kind === 'delta') return { ...item, summary: item.summary + mutation.event.delta, updatedAt: mutation.event.updatedAt };
        if (mutation.event.patch.status && isItemStatusRegression(item, { status: mutation.event.patch.status, updatedAt: mutation.event.patch.updatedAt })) return item;
        return { ...item, ...mutation.event.patch };
      };
      scope = { root: update(scope.root as Item), lifecycle: update(scope.lifecycle as Item),
        completion: scope.completion ? update(scope.completion as Item) : undefined };
    }
    if (!getItemById(event.itemId)) for (const ids of unheldMutations.values()) ids.add(event.itemId);
    if (mutation.kind === 'delta') stream.applyItemDelta(mutation.event);
    else if (mutation.kind === 'meta') stream.applyItemMeta(mutation.event);
    else stream.applyItemPatch(mutation.event);
    if (event.itemId === scope?.root.id || event.itemId === scope?.lifecycle.id || event.itemId === scope?.completion?.id) {
      contextVersion++;
      contextRefresh.request();
    }
  }
  function apply(observation: WindowObservation) {
    if (observation.kind !== 'snapshot') { applyMutation(observation); return; }
    if (observation.gone) { gone = true; generation++; return; }
    gone = false;
    const context = observation.scope ?? observation.page?.scope;
    if (context && (!scope || observation.contextVersion === contextVersion)) {
      scope = context;
      selection = { ...selection, scopeRootId: context.root.id };
    }
    const page = observation.page;
    if (!page) return;
    const current = new Map(getItems().map(item => [item.id, item]));
    const next = new Map((page.items as Item[]).map(item => [item.id, item]));
    for (const id of observation.touched) {
      const item = current.get(id);
      if (item) next.set(id, item);
      else next.delete(id);
    }
    installTimelineItems([...next.values()].sort(compareItemsByTimelinePosition), {
      disposeDropped: true, afterCommit: () => {
        window.applyWindowMetadataFromPaged({ ...page, scope: undefined });
        const changed = [...observation.touched].flatMap(id => current.get(id) ?? []);
        window.refreshCursorsAfterUpserts(changed, true, page.items as Item[]);
      },
    });
    if (observation.touched.size) for (const run of page.runs) runs.markRunDirty(run.firstItemId);
  }
  async function read(signal: AbortSignal): Promise<WindowObservation | null> {
    const gen = ++requestGeneration;
    const surfaceGeneration = generation;
    const windowRequest = window.requestVersion;
    const contextAtRead = contextVersion;
    loading = true;
    const touched = new Set<string>();
    reads.set(signal, touched);
    const unheld = new Set<string>();
    unheldMutations.set(signal, unheld);
    const ownership = threadBackend(thread.id);
    const backend = requireEntityBackend(ownership);
    if (!threadHasScope('threads:read', thread.id)) throw new Error('Thread history access is unavailable');
    const current = () => !disposed && !signal.aborted && generation === surfaceGeneration
      && requestGeneration === gen && threadBackend(thread.id) === ownership
      && window.requestVersion === windowRequest && !window.loadingOlder && !window.loadingNewer;
    const superseded = () => {
      if (!signal.aborted && !disposed && !gone && generation === surfaceGeneration) contextRefresh.request();
      return null;
    };
    await awaitBackendReplay(backend);
    if (!current()) return superseded();
    const saved = getThreadScrollSnapshot(key);
    const response = await readTimelineWindow(thread.id, selection, getItems(), window, runs,
      saved?.kind === 'anchor' ? saved.itemId : '', current);
    if (!current()) return superseded();
    if (response.page?.items.some(item => unheld.has(item.id))) missedStreamUpdate = true;
    return { kind: 'snapshot', page: response.page, scope: response.scope, gone: response.status === 'gone', touched, contextVersion: contextAtRead };
  }
  async function refresh() { await attachment?.refresh(); }
  let releaseSurface: (() => void) | undefined;
  let releaseIdentity: (() => void) | undefined;
  function start() {
    if (attachment || disposed) return;
    releaseSurface = registerTimelineSurface({ threadId: thread.id, backend: () => threadBackend(thread.id),
      apply: mutation => attachment?.apply(mutation), refresh });
    attachment = attachTimelineWindow(key, {
      backend: () => threadBackend(thread.id), read, apply,
      endRead: (signal, completed) => {
        reads.delete(signal);
        unheldMutations.delete(signal);
        if (reads.size === 0) {
          if (completed) loading = false;
          if (missedStreamUpdate && !disposed && !gone) contextRefresh.request();
          missedStreamUpdate = false;
        }
      },
    });
    releaseIdentity = onThreadHistoryInvalidated(owns => {
      if (!owns(thread.id)) return;
      generation++;
      reveal.disposeAll(); runs.clear(); installTimelineItems([], { disposeDropped: true });
      window.resetForFreshThread(); scope = null;
      void refresh().catch(error => reportFrontendDiagnostic('scoped timeline ownership refresh failed', errString(error)));
    });
  }
  return {
    start, refresh, itemWindow, rows, reveal, window, runs, scroll,
    get scope() { return scope; }, get loading() { return loading; }, get gone() { return gone; },
    get error() { return attachment?.error ?? null; },
    get generation() { return generation; }, get lastLiveContentAt() { return lastLiveContentAt; },
    dispose() {
      if (disposed) return;
      disposed = true; generation++; contextRefresh.dispose();
      releaseIdentity?.(); releaseSurface?.(); attachment?.release(); attachment = null;
      reveal.disposeAll(); runs.clear(); rows.clear(); installTimelineItems([], { disposeDropped: true });
    },
  };
}
