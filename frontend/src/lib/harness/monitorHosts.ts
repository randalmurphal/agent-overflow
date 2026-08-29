import type {
  CompositorMonitorSignal,
  MonitorContext,
  MonitorInstance,
  MonitorSpec,
  SourceMonitorSignal,
} from './monitorTypes';

interface EntryLike extends PerformanceEntry {
  hadRecentInput?: boolean;
  value?: number;
}

interface HostState {
  readonly context: MonitorContext;
  readonly startedAtMs: number;
  readonly cleanup: Array<() => void>;
  readonly counts: Record<string, number>;
  readonly totals: Record<string, number>;
  readonly maxima: Record<string, number>;
  readonly values: Record<string, unknown>;
  lastFrameAtMs: number | null;
  frameCount: number;
  skippedFrames: number;
  rafId: number | null;
  stopped: boolean;
  refresh?: () => void;
}

type SourceAdapter = { read: () => SourceMonitorSignal };
type CompositorAdapter = { read: () => CompositorMonitorSignal };

/** Installs an owned source/parser adapter for provider-side evidence. */
export function installSourceMonitorAdapter(read: () => SourceMonitorSignal): () => void {
  const host = globalThis as { __aoMonitorSourceSignals?: SourceAdapter };
  const previous = host.__aoMonitorSourceSignals;
  const adapter: SourceAdapter = { read };
  host.__aoMonitorSourceSignals = adapter;
  return () => {
    if (host.__aoMonitorSourceSignals !== adapter) return;
    if (previous) host.__aoMonitorSourceSignals = previous;
    else delete host.__aoMonitorSourceSignals;
  };
}

/** Installs an owned compositor adapter supplied by a capable platform host. */
export function installCompositorMonitorAdapter(read: () => CompositorMonitorSignal): () => void {
  const host = globalThis as { __aoMonitorCompositorCounters?: CompositorAdapter };
  const previous = host.__aoMonitorCompositorCounters;
  const adapter: CompositorAdapter = { read };
  host.__aoMonitorCompositorCounters = adapter;
  return () => {
    if (host.__aoMonitorCompositorCounters !== adapter) return;
    if (previous) host.__aoMonitorCompositorCounters = previous;
    else delete host.__aoMonitorCompositorCounters;
  };
}

/**
 * Creates the browser hosts for the built-in monitor specifications. These
 * hosts use browser observation APIs only. They never evaluate a caller's
 * string, inspect pixels, or retain DOM text.
 */
export function createBuiltinMonitor(spec: MonitorSpec, context: MonitorContext): MonitorInstance | undefined {
  const state = newState(context);
  switch (spec.id) {
    case 'frame-pacing':
    case 'skipped-frames':
    case 'animation-continuity':
      armFrames(state, spec.id);
      break;
    case 'long-task':
      armPerformanceObserver(state, 'longtask', 'longTask');
      break;
    case 'long-animation-frame':
      armPerformanceObserver(state, 'long-animation-frame', 'longAnimationFrame');
      break;
    case 'layout-shift':
      armPerformanceObserver(state, 'layout-shift', 'layoutShift', true);
      break;
    case 'input-to-render':
      armInputToRender(state);
      break;
    case 'scroll-response':
      armScrollResponse(state);
      break;
    case 'semantic-dom-stability':
      armSemanticDOM(state);
      break;
    case 'focus-clipping-settledness':
      armFocusClippingSettledness(state);
      break;
    case 'source-rewind':
    case 'parser-boundary':
      armSourceAdapter(state, spec.id);
      break;
    case 'compositor-counters':
      armCompositorAdapter(state);
      break;
  }
  return {
    heartbeat: (atMs) => {
      if (state.stopped) return;
      state.refresh?.();
      context.observe(summary(state, atMs));
    },
    stop: (atMs) => {
      if (!state.stopped) {
        state.stopped = true;
        for (const remove of state.cleanup.splice(0)) {
          try {
            remove();
          } catch (error) {
            context.fail(`monitor cleanup failed: ${error instanceof Error ? error.message : String(error)}`);
          }
        }
      }
      return summary(state, atMs);
    },
  };
}

function newState(context: MonitorContext): HostState {
  return {
    context,
    startedAtMs: context.startedAtMs,
    cleanup: [],
    counts: Object.create(null) as Record<string, number>,
    totals: Object.create(null) as Record<string, number>,
    maxima: Object.create(null) as Record<string, number>,
    values: Object.create(null) as Record<string, unknown>,
    lastFrameAtMs: null,
    frameCount: 0,
    skippedFrames: 0,
    rafId: null,
    stopped: false,
  };
}

function increment(state: HostState, key: string, value = 1): void {
  state.counts[key] = (state.counts[key] ?? 0) + value;
}

function recordDuration(state: HostState, key: string, value: number): void {
  if (!Number.isFinite(value) || value < 0) return;
  increment(state, key);
  state.totals[key] = (state.totals[key] ?? 0) + value;
  state.maxima[key] = Math.max(state.maxima[key] ?? 0, value);
}

function armFrames(state: HostState, monitorId: string): void {
  if (typeof requestAnimationFrame !== 'function') {
    state.context.fail('requestAnimationFrame is unavailable');
    return;
  }
  const tick = (atMs: number): void => {
    if (state.stopped) return;
    if (state.lastFrameAtMs !== null) {
      const delta = atMs - state.lastFrameAtMs;
      if (Number.isFinite(delta) && delta >= 0) {
        state.frameCount += 1;
        if (monitorId === 'frame-pacing') recordDuration(state, 'frameMs', delta);
        if (monitorId === 'skipped-frames') {
          state.skippedFrames += Math.max(0, Math.round(delta / 16.667) - 1);
        }
        if (monitorId === 'animation-continuity' && delta > 100) increment(state, 'continuityBreaks');
      }
    }
    state.lastFrameAtMs = atMs;
    state.rafId = requestAnimationFrame(tick);
  };
  state.rafId = requestAnimationFrame(tick);
  state.cleanup.push(() => {
    if (state.rafId !== null) cancelAnimationFrame(state.rafId);
    state.rafId = null;
  });
}

function armPerformanceObserver(state: HostState, entryType: string, key: string, layoutShift = false): void {
  if (typeof PerformanceObserver === 'undefined' || !PerformanceObserver.supportedEntryTypes?.includes(entryType)) {
    state.context.fail(`${entryType} PerformanceObserver entries are unavailable`);
    return;
  }
  try {
    const observer = new PerformanceObserver((list) => {
      for (const raw of list.getEntries()) {
        const entry = raw as EntryLike;
        if (layoutShift && entry.hadRecentInput) continue;
        if (layoutShift) state.values.cumulativeLayoutShift = (Number(state.values.cumulativeLayoutShift) || 0) + (entry.value ?? 0);
        else recordDuration(state, key, entry.duration);
      }
    });
    observer.observe({ type: entryType, buffered: false });
    state.cleanup.push(() => observer.disconnect());
  } catch (error) {
    state.context.fail(`${entryType} observer failed: ${error instanceof Error ? error.message : String(error)}`);
  }
}

function armInputToRender(state: HostState): void {
  if (typeof document === 'undefined' || typeof requestAnimationFrame !== 'function') {
    state.context.fail('input-to-render needs document and requestAnimationFrame');
    return;
  }
  let pendingAt: number | null = null;
  let pendingFrame: number | null = null;
  const onInput = (): void => {
    if (pendingAt !== null) return;
    pendingAt = performance.now();
    pendingFrame = requestAnimationFrame(() => {
      if (pendingAt === null) return;
      recordDuration(state, 'inputToRenderMs', performance.now() - pendingAt);
      pendingAt = null;
      pendingFrame = null;
    });
  };
  for (const type of ['pointerdown', 'keydown', 'input', 'click']) document.addEventListener(type, onInput, { passive: true });
  state.cleanup.push(() => {
    for (const type of ['pointerdown', 'keydown', 'input', 'click']) document.removeEventListener(type, onInput);
    if (pendingFrame !== null) cancelAnimationFrame(pendingFrame);
    pendingFrame = null;
    pendingAt = null;
  });
}

function armScrollResponse(state: HostState): void {
  if (typeof document === 'undefined' || typeof requestAnimationFrame !== 'function') {
    state.context.fail('scroll-response needs document and requestAnimationFrame');
    return;
  }
  let pendingAt: number | null = null;
  let pendingFrame: number | null = null;
  const onScroll = (): void => {
    if (pendingAt !== null) return;
    pendingAt = performance.now();
    pendingFrame = requestAnimationFrame(() => {
      if (pendingAt === null) return;
      recordDuration(state, 'scrollToRenderMs', performance.now() - pendingAt);
      pendingAt = null;
      pendingFrame = null;
    });
  };
  document.addEventListener('scroll', onScroll, { passive: true, capture: true });
  state.cleanup.push(() => document.removeEventListener('scroll', onScroll, true));
  state.cleanup.push(() => {
    if (pendingFrame !== null) cancelAnimationFrame(pendingFrame);
    pendingFrame = null;
    pendingAt = null;
  });
}

function armSemanticDOM(state: HostState): void {
  if (typeof MutationObserver === 'undefined' || typeof document === 'undefined') {
    state.context.fail('semantic DOM stability needs MutationObserver and document');
    return;
  }
  const identities = new Map<string, Element>();
  const scan = (): void => {
    const current = new Map<string, Element>();
    for (const element of document.querySelectorAll('[data-item-id]')) {
      const id = element.getAttribute('data-item-id');
      if (!id) continue;
      const previous = identities.get(id);
      if (previous && previous !== element) increment(state, 'identityReplacements');
      current.set(id, element);
    }
    identities.clear();
    for (const [id, element] of current) identities.set(id, element);
    state.values.semanticIdentityCount = identities.size;
  };
  scan();
  const observer = new MutationObserver((records) => {
    for (const record of records) {
      increment(state, 'mutations');
      if (record.type === 'childList') {
        increment(state, 'mountChanges', record.addedNodes.length + record.removedNodes.length);
      }
    }
    scan();
  });
  observer.observe(document.documentElement, { subtree: true, childList: true, attributes: true, attributeFilter: ['data-item-id'] });
  state.cleanup.push(() => observer.disconnect());
}

function armFocusClippingSettledness(state: HostState): void {
  if (typeof document === 'undefined') {
    state.context.fail('focus-clipping-settledness needs document');
    return;
  }
  let lastMutationAt = performance.now();
  const observer = typeof MutationObserver === 'undefined' ? null : new MutationObserver(() => { lastMutationAt = performance.now(); });
  observer?.observe(document.documentElement, { subtree: true, childList: true, attributes: true, characterData: true });
  const recordFocus = (): void => {
    const active = document.activeElement;
    if (active) increment(state, 'focusChanges');
  };
  const recordClipping = (): void => {
    const active = document.activeElement;
    if (active instanceof HTMLElement) {
      const rect = active.getBoundingClientRect();
      const clipped = rect.bottom < 0 || rect.top > window.innerHeight;
      if (clipped) increment(state, 'clippedFocus');
    }
  };
  document.addEventListener('focusin', recordFocus, { passive: true });
  state.refresh = () => {
    state.values.settled = performance.now() - lastMutationAt >= 300;
    recordClipping();
  };
  state.cleanup.push(() => {
    observer?.disconnect();
    document.removeEventListener('focusin', recordFocus);
  });
}

function armSourceAdapter(state: HostState, monitorId: string): void {
  const source = (globalThis as { __aoMonitorSourceSignals?: { read: () => SourceMonitorSignal } }).__aoMonitorSourceSignals;
  if (!source || typeof source.read !== 'function') {
    state.context.fail(`${monitorId} source adapter is unavailable`);
    return;
  }
  let lastCursor: number | null = null;
  let lastBoundary: number | null = null;
  const sample = (): void => {
    try {
      const signal = source.read();
      if (!signal || typeof signal !== 'object' || !Number.isFinite(signal.cursor)) throw new Error('source signal has no finite cursor');
      if (monitorId === 'parser-boundary' && !Number.isFinite(signal.parserBoundaries)) throw new Error('parser signal has no finite boundary count');
      if (lastCursor !== null && signal.cursor < lastCursor) increment(state, 'rewinds');
      if (signal.parserBoundaries !== undefined) {
        if (!Number.isFinite(signal.parserBoundaries) || signal.parserBoundaries < 0) throw new Error('source signal has an invalid parser boundary count');
        if (lastBoundary !== null && signal.parserBoundaries < lastBoundary) increment(state, 'parserBoundaryRewinds');
        lastBoundary = signal.parserBoundaries;
      }
      lastCursor = signal.cursor;
      state.values[monitorId] = signal;
      increment(state, 'sourceSamples');
    } catch (error) {
      state.context.fail(`${monitorId} source adapter failed: ${error instanceof Error ? error.message : String(error)}`);
    }
  };
  sample();
  state.refresh = sample;
}

function armCompositorAdapter(state: HostState): void {
  const adapter = (globalThis as { __aoMonitorCompositorCounters?: { read: () => CompositorMonitorSignal } }).__aoMonitorCompositorCounters;
  if (!adapter || typeof adapter.read !== 'function') {
    state.context.fail('compositor counter adapter is unavailable on this platform');
    return;
  }
  state.refresh = () => {
    try {
      const signal = adapter.read();
      if (!signal || typeof signal !== 'object') throw new Error('compositor signal is not an object');
      state.values.compositor = signal;
      increment(state, 'compositorSamples');
    } catch (error) {
      state.context.fail(`compositor counter adapter failed: ${error instanceof Error ? error.message : String(error)}`);
    }
  };
  state.refresh();
}

function summary(state: HostState, atMs: number): Record<string, unknown> {
  const result: Record<string, unknown> = {
    atMs,
    durationMs: Math.max(0, atMs - state.startedAtMs),
    counts: { ...state.counts, skippedFrames: state.skippedFrames, frames: state.frameCount },
    totals: { ...state.totals },
    maxima: { ...state.maxima },
  };
  for (const [key, value] of Object.entries(state.values)) {
    if (typeof value !== 'function' && !(value instanceof Set)) result[key] = value;
  }
  return result;
}
