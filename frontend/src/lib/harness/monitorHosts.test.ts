import { afterEach, describe, expect, it, vi } from 'vitest';
import { createBuiltinMonitor, installCompositorMonitorAdapter, installSourceMonitorAdapter } from './monitorHosts';
import type { MonitorCapability, MonitorContext, MonitorSpec } from './monitorTypes';

const contexts: MonitorContext[] = [];

function context(id: string, capabilities: readonly MonitorCapability[]): { context: MonitorContext; observations: unknown[]; errors: string[] } {
  const observations: unknown[] = [];
  const errors: string[] = [];
  const monitorContext: MonitorContext = {
    runId: 'host-test', monitorId: id, startedAtMs: 0,
    capabilities: new Set(capabilities),
    probes: { read: (request) => ({ kind: request.kind, atMs: 0, available: false }) },
    now: () => 0,
    observe: (value) => observations.push(value),
    fail: (message) => errors.push(message),
  };
  contexts.push(monitorContext);
  return { context: monitorContext, observations, errors };
}

function spec(id: string, requires: readonly MonitorCapability[]): MonitorSpec {
  return { v: 1, id, title: id, description: id, compatibilityLeg: 'instrumented-renderer', perturbation: 'none', requires };
}

afterEach(() => {
  vi.unstubAllGlobals();
  document.body.innerHTML = '';
  contexts.length = 0;
});

describe('browser monitor hosts', () => {
  it('records frame pacing and skipped-frame faults from injected rAF timestamps', () => {
    const callbacks = new Map<number, FrameRequestCallback>();
    let nextID = 1;
    vi.stubGlobal('requestAnimationFrame', (callback: FrameRequestCallback) => { const id = nextID++; callbacks.set(id, callback); return id; });
    vi.stubGlobal('cancelAnimationFrame', (id: number) => { callbacks.delete(id); });
    const frame = context('frame-pacing', ['animation-frame']);
    const host = createBuiltinMonitor(spec('frame-pacing', ['animation-frame']), frame.context)!;
    callbacks.get(1)?.(0);
    callbacks.get(2)?.(20);
    host.heartbeat?.(30);
    const frameSummary = frame.observations[0] as { counts: { frameMs: number }; maxima: { frameMs: number } };
    expect(frameSummary.counts.frameMs).toBe(1);
    expect(frameSummary.maxima.frameMs).toBe(20);
    host.stop?.(30);

    const skipped = context('skipped-frames', ['animation-frame']);
    const skippedHost = createBuiltinMonitor(spec('skipped-frames', ['animation-frame']), skipped.context)!;
    callbacks.get(4)?.(0);
    callbacks.get(5)?.(70);
    const summary = skippedHost.stop?.(70) as { counts: { skippedFrames: number } };
    expect(summary.counts.skippedFrames).toBe(3);
  });

  it('observes semantic remounts without retaining row text', async () => {
    document.body.innerHTML = '<div data-item-id="row-1">first</div>';
    const capture = context('semantic-dom-stability', ['dom', 'semantic-dom']);
    const host = createBuiltinMonitor(spec('semantic-dom-stability', ['dom', 'semantic-dom']), capture.context)!;
    document.body.innerHTML = '<div data-item-id="row-1">second</div>';
    await Promise.resolve();
    const summary = host.stop?.(10) as { counts: { identityReplacements: number }; semanticIdentityCount: number };
    expect(summary.counts.identityReplacements).toBe(1);
    expect(summary.semanticIdentityCount).toBe(1);
    expect(JSON.stringify(summary)).not.toContain('first');
  });

  it('keeps semantic identity counts current as rows mount and unmount', async () => {
    document.body.innerHTML = '<div data-item-id="row-1">one</div>';
    const capture = context('semantic-dom-stability', ['dom', 'semantic-dom']);
    const host = createBuiltinMonitor(spec('semantic-dom-stability', ['dom', 'semantic-dom']), capture.context)!;
    document.body.innerHTML += '<div data-item-id="row-2">two</div>';
    await Promise.resolve();
    host.heartbeat?.(10);
    expect((capture.observations.at(-1) as { semanticIdentityCount: number }).semanticIdentityCount).toBe(2);
    document.querySelector('[data-item-id="row-1"]')?.remove();
    await Promise.resolve();
    const summary = host.stop?.(20) as { semanticIdentityCount: number };
    expect(summary.semanticIdentityCount).toBe(1);
  });

  it('counts focus events once instead of once per heartbeat', () => {
    document.body.innerHTML = '<input data-testid="focus" />';
    const capture = context('focus-clipping-settledness', ['dom', 'focus']);
    const host = createBuiltinMonitor(spec('focus-clipping-settledness', ['dom', 'focus']), capture.context)!;
    document.dispatchEvent(new Event('focusin'));
    host.heartbeat?.(10);
    host.heartbeat?.(20);
    const summary = host.stop?.(30) as { counts: { focusChanges: number } };
    expect(summary.counts.focusChanges).toBe(1);
  });

  it('records input-to-render and scroll-to-render latency through typed event hosts', () => {
    const callbacks = new Map<number, FrameRequestCallback>();
    let nextID = 1;
    vi.stubGlobal('requestAnimationFrame', (callback: FrameRequestCallback) => { const id = nextID++; callbacks.set(id, callback); return id; });
    vi.stubGlobal('cancelAnimationFrame', (id: number) => { callbacks.delete(id); });
    const input = context('input-to-render', ['input', 'animation-frame']);
    const inputHost = createBuiltinMonitor(spec('input-to-render', ['input', 'animation-frame']), input.context)!;
    document.dispatchEvent(new Event('input'));
    callbacks.get(1)?.(1);
    expect((inputHost.stop?.(2) as { counts: { inputToRenderMs: number } }).counts.inputToRenderMs).toBe(1);

    const scroll = context('scroll-response', ['scroll', 'animation-frame']);
    const scrollHost = createBuiltinMonitor(spec('scroll-response', ['scroll', 'animation-frame']), scroll.context)!;
    document.dispatchEvent(new Event('scroll'));
    callbacks.get(2)?.(3);
    expect((scrollHost.stop?.(4) as { counts: { scrollToRenderMs: number } }).counts.scrollToRenderMs).toBe(1);
  });

  it('records long-task entries and observer setup faults', () => {
    class FakeObserver {
      static supportedEntryTypes = ['longtask'];
      static current: FakeObserver | undefined;
      readonly callback: (list: { getEntries: () => PerformanceEntry[] }) => void;
      constructor(callback: (list: { getEntries: () => PerformanceEntry[] }) => void) { this.callback = callback; FakeObserver.current = this; }
      observe(): void {}
      disconnect(): void {}
    }
    vi.stubGlobal('PerformanceObserver', FakeObserver);
    const capture = context('long-task', ['performance:longtask']);
    const host = createBuiltinMonitor(spec('long-task', ['performance:longtask']), capture.context)!;
    FakeObserver.current?.callback({ getEntries: () => [{ duration: 72 } as PerformanceEntry] });
    const summary = host.stop?.(100) as { counts: { longTask: number }; maxima: { longTask: number } };
    expect(summary.counts.longTask).toBe(1);
    expect(summary.maxima.longTask).toBe(72);
  });

  it('records layout shifts while excluding shifts with recent input', () => {
    class FakeObserver {
      static supportedEntryTypes = ['layout-shift'];
      static current: FakeObserver | undefined;
      readonly callback: (list: { getEntries: () => PerformanceEntry[] }) => void;
      constructor(callback: (list: { getEntries: () => PerformanceEntry[] }) => void) { this.callback = callback; FakeObserver.current = this; }
      observe(): void {}
      disconnect(): void {}
    }
    vi.stubGlobal('PerformanceObserver', FakeObserver);
    const capture = context('layout-shift', ['performance:layout-shift']);
    const host = createBuiltinMonitor(spec('layout-shift', ['performance:layout-shift']), capture.context)!;
    FakeObserver.current?.callback({ getEntries: () => [
      { value: 0.25, hadRecentInput: false } as unknown as PerformanceEntry,
      { value: 0.5, hadRecentInput: true } as unknown as PerformanceEntry,
    ] });
    const summary = host.stop?.(100) as { cumulativeLayoutShift: number };
    expect(summary.cumulativeLayoutShift).toBe(0.25);
  });

  it('uses typed source and compositor adapters and refuses malformed source data', () => {
    let cursor = 4;
    const removeSource = installSourceMonitorAdapter(() => ({ cursor, parserBoundaries: cursor }));
    const source = context('source-rewind', ['source-rewind']);
    const sourceHost = createBuiltinMonitor(spec('source-rewind', ['source-rewind']), source.context)!;
    cursor = 2;
    sourceHost.heartbeat?.(10);
    const sourceSummary = sourceHost.stop?.(20) as { 'source-rewind': { cursor: number }; counts: { rewinds: number } };
    expect(sourceSummary['source-rewind'].cursor).toBe(2);
    expect(sourceSummary.counts.rewinds).toBe(1);
    removeSource();

    const removeCompositor = installCompositorMonitorAdapter(() => ({ presentedFrames: 2, droppedFrames: 1 }));
    const compositor = context('compositor-counters', ['compositor-counters']);
    const compositorHost = createBuiltinMonitor(spec('compositor-counters', ['compositor-counters']), compositor.context)!;
    const compositorSummary = compositorHost.stop?.(20) as { compositor: { droppedFrames: number } };
    expect(compositorSummary.compositor.droppedFrames).toBe(1);
    removeCompositor();
  });

  it('restores a previous adapter after nested replacement cleanup', () => {
    const removeFirst = installSourceMonitorAdapter(() => ({ cursor: 1 }));
    const removeSecond = installSourceMonitorAdapter(() => ({ cursor: 2 }));
    removeSecond();
    const source = context('source-rewind', ['source-rewind']);
    const host = createBuiltinMonitor(spec('source-rewind', ['source-rewind']), source.context)!;
    const summary = host.stop?.(10) as { 'source-rewind': { cursor: number } };
    expect(summary['source-rewind'].cursor).toBe(1);
    removeFirst();
  });
});
