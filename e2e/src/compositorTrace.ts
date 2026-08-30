import type { Page } from '@playwright/test';

interface TraceEvent {
  readonly name?: string;
  readonly ph?: string;
  readonly ts?: number;
  readonly dur?: number;
  readonly args?: unknown;
}

interface TracingComplete {
  readonly stream?: string;
}

export interface CompositorSummary {
  readonly eventCount: number;
  readonly renderPasses: number;
  readonly prepareDraws: number;
  readonly layerTreeSnapshots: number;
  readonly rasterTasks: number;
  readonly rasterOverlappingDraws: number;
  readonly missingTileSignals: number;
  readonly checkerboardSignals: number;
  readonly blankRenderPasses: number;
}

export interface CompositorTrace {
  stop(): Promise<readonly TraceEvent[]>;
}

const TRACE_CATEGORIES = [
  'benchmark',
  'blink.user_timing',
  'cc',
  'disabled-by-default-cc.debug',
  'devtools.timeline',
  'disabled-by-default-devtools.timeline.frame',
  'viz',
];

export async function startCompositorTrace(page: Page): Promise<CompositorTrace> {
  const cdp = await page.context().newCDPSession(page);
  let stopped = false;
  const complete = new Promise<TracingComplete>((resolve) => {
    cdp.once('Tracing.tracingComplete', (event) => resolve(event as TracingComplete));
  });
  try {
    await cdp.send('Tracing.start', {
      traceConfig: { includedCategories: TRACE_CATEGORIES, excludedCategories: ['*'] },
      transferMode: 'ReturnAsStream',
    });
  } catch (error) {
    await cdp.detach();
    throw error;
  }

  return {
    async stop() {
      if (stopped) throw new Error('compositor trace stopped twice');
      stopped = true;
      try {
        await cdp.send('Tracing.end');
        const result = await complete;
        if (!result.stream) throw new Error('compositor trace completed without a stream');
        let data = '';
        for (;;) {
          const chunk = await cdp.send('IO.read', {
            handle: result.stream,
            size: 8_000_000,
          }) as { data?: string; eof?: boolean };
          data += chunk.data ?? '';
          if (chunk.eof) break;
        }
        await cdp.send('IO.close', { handle: result.stream });
        const parsed: unknown = JSON.parse(data);
        if (Array.isArray(parsed)) return parsed as TraceEvent[];
        if (parsed && typeof parsed === 'object' && 'traceEvents' in parsed) {
          const events = (parsed as { traceEvents?: unknown }).traceEvents;
          if (Array.isArray(events)) return events as TraceEvent[];
        }
        throw new Error('compositor trace has no traceEvents array');
      } finally {
        await cdp.detach();
      }
    },
  };
}

interface SignalCounts {
  missing: number;
  checkerboard: number;
  blank: number;
}

function signalCounts(value: unknown, path = '', depth = 0): SignalCounts {
  if (value === null || typeof value !== 'object' || depth > 8) {
    return { missing: 0, checkerboard: 0, blank: 0 };
  }
  if (Array.isArray(value)) {
    return value.reduce((total, item) => {
      const child = signalCounts(item, path, depth + 1);
      total.missing += child.missing;
      total.checkerboard += child.checkerboard;
      total.blank += child.blank;
      return total;
    }, { missing: 0, checkerboard: 0, blank: 0 });
  }
  const total = { missing: 0, checkerboard: 0, blank: 0 };
  for (const [key, child] of Object.entries(value)) {
    const childPath = `${path} ${key}`.toLowerCase().replaceAll('_', ' ');
    if (typeof child === 'number') {
      if (child > 0 && /missing tile/.test(childPath)) total.missing += child;
      if (child > 0 && /checkerboard/.test(childPath)) total.checkerboard += child;
      if (child > 0 && /blank render/.test(childPath)) total.blank += child;
    } else {
      const nested = signalCounts(child, childPath, depth + 1);
      total.missing += nested.missing;
      total.checkerboard += nested.checkerboard;
      total.blank += nested.blank;
    }
  }
  return total;
}

function inTraceWindow(event: TraceEvent, start: number, end: number): boolean {
  return typeof event.ts === 'number' && event.ts >= start && event.ts <= end;
}

function markTimestamp(events: readonly TraceEvent[], name: string): number {
  const event = events.find((candidate) => candidate.name === name && typeof candidate.ts === 'number');
  if (event?.ts === undefined) throw new Error(`compositor trace is missing user-timing mark ${name}`);
  return event.ts;
}

export function summarizeCompositorWindow(
  events: readonly TraceEvent[],
  startMark: string,
  endMark: string,
): CompositorSummary {
  const start = markTimestamp(events, startMark);
  const end = markTimestamp(events, endMark);
  if (end <= start) throw new Error(`compositor trace marks are out of order: ${startMark}, ${endMark}`);
  const window = events.filter((event) => inTraceWindow(event, start, end));
  const renderPasses = window.filter((event) => event.name === 'LayerTreeHostImpl::CalculateRenderPasses').length;
  const prepareDraws = window.filter((event) => event.name === 'LayerTreeHostImpl::PrepareToDraw').length;
  const layerTreeSnapshots = window.filter((event) => event.name === 'LayerTreeHostImpl:snapshot').length;
  const raster = window.filter((event) =>
    typeof event.ts === 'number' && typeof event.dur === 'number'
      && /RasterizerTaskImpl::RunOnWorkerThread|RasterTask/.test(event.name ?? ''),
  );
  const prepare = window.filter((event) => event.name === 'LayerTreeHostImpl::PrepareToDraw');
  const rasterOverlappingDraws = prepare.filter((draw) => raster.some((task) =>
    draw.ts !== undefined && task.ts !== undefined && task.dur !== undefined
      && draw.ts >= task.ts && draw.ts <= task.ts + task.dur,
  )).length;
  let missingTileSignals = 0;
  let checkerboardSignals = 0;
  let blankRenderPasses = 0;
  for (const event of window) {
    const name = (event.name ?? '').toLowerCase();
    const metrics = signalCounts(event.args);
    missingTileSignals += metrics.missing;
    checkerboardSignals += metrics.checkerboard;
    blankRenderPasses += metrics.blank;
    if (/missing.?tile|checkerboard|blank.?render.?pass/i.test(event.name ?? '')) {
      if (metrics.missing + metrics.checkerboard + metrics.blank === 0) {
        if (/missing.?tile/i.test(event.name ?? '')) missingTileSignals += 1;
        if (/checkerboard/i.test(event.name ?? '')) checkerboardSignals += 1;
        if (/blank.?render.?pass/i.test(event.name ?? '')) blankRenderPasses += 1;
      }
    }
  }
  return {
    eventCount: window.length,
    renderPasses,
    prepareDraws,
    layerTreeSnapshots,
    rasterTasks: raster.length,
    rasterOverlappingDraws,
    missingTileSignals,
    checkerboardSignals,
    blankRenderPasses,
  };
}
