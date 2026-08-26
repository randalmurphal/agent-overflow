import { afterEach, describe, expect, it, vi } from 'vitest';
import { installLoafTrace } from './loafTrace';
import {
  clearUiRenderTrace,
  getUiRenderTraceRecords,
  setUiRenderTraceEnabled,
} from './uiRenderTrace';

type ObserverCallback = (list: { getEntries(): unknown[] }) => void;

class FakePerformanceObserver {
  static supportedEntryTypes: string[] = ['long-animation-frame'];
  static instances: FakePerformanceObserver[] = [];
  static observeError: Error | null = null;

  callback: ObserverCallback;
  observedWith: unknown = null;
  disconnected = false;

  constructor(callback: ObserverCallback) {
    this.callback = callback;
    FakePerformanceObserver.instances.push(this);
  }

  observe(options: unknown): void {
    if (FakePerformanceObserver.observeError) throw FakePerformanceObserver.observeError;
    this.observedWith = options;
  }

  disconnect(): void {
    this.disconnected = true;
  }

  emit(entries: unknown[]): void {
    this.callback({ getEntries: () => entries });
  }
}

function makeScript(overrides: Record<string, unknown> = {}): Record<string, unknown> {
  return {
    invoker: 'FrameRequestCallback',
    invokerType: 'user-callback',
    sourceURL: 'http://localhost:5173/src/lib/utils/scroll/spring.ts?t=123',
    sourceFunctionName: 'tick',
    duration: 12,
    forcedStyleAndLayoutDuration: 0,
    ...overrides,
  };
}

function makeLoafEntry(overrides: Record<string, unknown> = {}): Record<string, unknown> {
  return {
    startTime: 1000.25,
    duration: 72.5,
    blockingDuration: 21.4,
    renderStart: 1050.5,
    styleAndLayoutStart: 1060,
    firstUIEventTimestamp: 990.1,
    scripts: [makeScript()],
    ...overrides,
  };
}

function installFake(): void {
  vi.stubGlobal('PerformanceObserver', FakePerformanceObserver);
}

describe('loafTrace', () => {
  afterEach(() => {
    clearUiRenderTrace();
    setUiRenderTraceEnabled(false);
    FakePerformanceObserver.instances = [];
    FakePerformanceObserver.supportedEntryTypes = ['long-animation-frame'];
    FakePerformanceObserver.observeError = null;
    vi.unstubAllGlobals();
  });

  it('registers nothing while tracing is disabled', () => {
    installFake();
    setUiRenderTraceEnabled(false);

    const cleanup = installLoafTrace();

    expect(FakePerformanceObserver.instances).toHaveLength(0);
    expect(getUiRenderTraceRecords()).toEqual([]);
    cleanup();
  });

  it('observes long-animation-frame buffered and records the install', () => {
    installFake();
    setUiRenderTraceEnabled(true);

    installLoafTrace();

    expect(FakePerformanceObserver.instances).toHaveLength(1);
    expect(FakePerformanceObserver.instances[0]?.observedWith).toEqual({
      type: 'long-animation-frame',
      buffered: true,
    });
    const records = getUiRenderTraceRecords();
    expect(records).toHaveLength(1);
    expect(records[0]?.label).toBe('frame.loaf.install');
    expect(records[0]?.data).toEqual({ supported: true });
  });

  it('records install with supported:false when the entry type is missing', () => {
    installFake();
    FakePerformanceObserver.supportedEntryTypes = ['longtask'];
    setUiRenderTraceEnabled(true);

    installLoafTrace();

    expect(FakePerformanceObserver.instances).toHaveLength(0);
    const records = getUiRenderTraceRecords();
    expect(records).toHaveLength(1);
    expect(records[0]?.data).toEqual({ supported: false });
  });

  it('records the observe failure instead of throwing', () => {
    installFake();
    FakePerformanceObserver.observeError = new Error('observe rejected');
    setUiRenderTraceEnabled(true);

    installLoafTrace();

    const records = getUiRenderTraceRecords();
    expect(records).toHaveLength(1);
    expect(records[0]?.label).toBe('frame.loaf.install');
    expect(records[0]?.data).toEqual({
      supported: false,
      observeError: 'observe rejected',
    });
  });

  it('maps entries into frame.loaf records', () => {
    installFake();
    setUiRenderTraceEnabled(true);
    installLoafTrace();

    FakePerformanceObserver.instances[0]?.emit([makeLoafEntry()]);

    const loaf = getUiRenderTraceRecords().filter((r) => r.label === 'frame.loaf');
    expect(loaf).toHaveLength(1);
    expect(loaf[0]?.data).toEqual({
      startTime: 1000.3,
      durationMs: 72.5,
      blockingMs: 21.4,
      renderStart: 1050.5,
      styleAndLayoutStart: 1060,
      firstUIEventTimestamp: 990.1,
      scriptCount: 1,
      scripts: [{
        invoker: 'FrameRequestCallback',
        invokerType: 'user-callback',
        source: 'scroll/spring.ts',
        fn: 'tick',
        durationMs: 12,
        forcedStyleLayoutMs: 0,
      }],
    });
  });

  it('keeps the top scripts by duration and reports the full count', () => {
    installFake();
    setUiRenderTraceEnabled(true);
    installLoafTrace();

    const scripts = [5, 40, 10, 25, 15].map((duration) =>
      makeScript({ duration, sourceFunctionName: `fn${duration}` }));
    FakePerformanceObserver.instances[0]?.emit([makeLoafEntry({ scripts })]);

    const loaf = getUiRenderTraceRecords().filter((r) => r.label === 'frame.loaf');
    const data = loaf[0]?.data as {
      scriptCount: number;
      scripts: { fn: string; durationMs: number }[];
    };
    expect(data.scriptCount).toBe(5);
    expect(data.scripts.map((s) => s.durationMs)).toEqual([40, 25, 15]);
  });

  it('stamps heap before/now when performance.memory is readable', () => {
    installFake();
    setUiRenderTraceEnabled(true);
    const memory = { usedJSHeapSize: 300 * 1048576 };
    vi.stubGlobal('performance', { now: () => 500, memory });
    const cleanup = installLoafTrace();

    // Install sampled 300MB at t=500; a GC then drops the heap before
    // the LoAF entry (startTime 1000) is delivered.
    memory.usedJSHeapSize = 180 * 1048576;
    FakePerformanceObserver.instances[0]?.emit([makeLoafEntry({ startTime: 1000 })]);

    const loaf = getUiRenderTraceRecords().filter((r) => r.label === 'frame.loaf');
    const data = loaf[0]?.data as { heapBeforeMb: number; heapNowMb: number };
    expect(data.heapBeforeMb).toBe(300);
    expect(data.heapNowMb).toBe(180);
    cleanup();
  });

  it('omits heap fields when performance.memory is absent', () => {
    installFake();
    setUiRenderTraceEnabled(true);
    const cleanup = installLoafTrace();

    FakePerformanceObserver.instances[0]?.emit([makeLoafEntry()]);

    const loaf = getUiRenderTraceRecords().filter((r) => r.label === 'frame.loaf');
    const data = loaf[0]?.data as Record<string, unknown>;
    expect('heapNowMb' in data && data.heapNowMb !== undefined).toBe(false);
    expect('heapBeforeMb' in data && data.heapBeforeMb !== undefined).toBe(false);
    cleanup();
  });

  it('cleanup disconnects the observer', () => {
    installFake();
    setUiRenderTraceEnabled(true);
    const cleanup = installLoafTrace();

    cleanup();

    expect(FakePerformanceObserver.instances[0]?.disconnected).toBe(true);
  });
});
