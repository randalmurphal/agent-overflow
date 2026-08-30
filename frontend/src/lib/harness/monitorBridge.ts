import { defaultMonitorRegistry } from './monitorRegistry';
import type { MonitorCapability, MonitorCompatibilityLeg, MonitorProbeReader, MonitorProbeResult } from './monitorTypes';

function fail(message: string): { error: string } {
  return { error: message };
}

function str(spec: Record<string, unknown>, key: string): string {
  const raw = spec[key];
  return typeof raw === 'string' ? raw : '';
}

function num(spec: Record<string, unknown>, key: string, fallback: number): number {
  if (!(key in spec)) return fallback;
  const raw = spec[key];
  if (typeof raw !== 'number' || !Number.isFinite(raw)) throw new Error(`monitor field ${JSON.stringify(key)} must be a finite number`);
  return raw;
}

function monitorCapabilities(): ReadonlySet<MonitorCapability> {
  const capabilities = new Set<MonitorCapability>();
  if (typeof requestAnimationFrame === 'function') capabilities.add('animation-frame');
  if (typeof document !== 'undefined') {
    capabilities.add('dom');
    capabilities.add('input');
    capabilities.add('scroll');
    capabilities.add('semantic-dom');
    capabilities.add('focus');
  }
  const supported = typeof PerformanceObserver === 'undefined'
    ? []
    : PerformanceObserver.supportedEntryTypes ?? [];
  if (supported.includes('longtask')) capabilities.add('performance:longtask');
  if (supported.includes('long-animation-frame')) capabilities.add('performance:long-animation-frame');
  if (supported.includes('event')) capabilities.add('performance:event');
  if (supported.includes('layout-shift')) capabilities.add('performance:layout-shift');
  const host = globalThis as {
    __aoMonitorSourceSignals?: { read?: unknown };
    __aoMonitorCompositorCounters?: { read?: unknown };
  };
  if (typeof host.__aoMonitorSourceSignals?.read === 'function') {
    capabilities.add('source-rewind');
    capabilities.add('parser-boundary');
  }
  if (typeof host.__aoMonitorCompositorCounters?.read === 'function') capabilities.add('compositor-counters');
  return capabilities;
}

// The monitor API receives this finite, typed reader rather than an eval
// escape hatch. Custom monitors can ask for structural browser facts while
// the bridge retains control of the actual DOM and platform operations.
function monitorProbes(): MonitorProbeReader {
  return {
    read: (request): MonitorProbeResult => {
      const atMs = typeof performance !== 'undefined' ? performance.now() : Date.now();
      switch (request.kind) {
        case 'frame':
        case 'animation':
          return { atMs, kind: request.kind, available: typeof requestAnimationFrame === 'function' };
        case 'long-task':
          return { atMs, kind: request.kind, available: typeof PerformanceObserver !== 'undefined' && PerformanceObserver.supportedEntryTypes?.includes('longtask') };
        case 'long-animation-frame':
          return { atMs, kind: request.kind, available: typeof PerformanceObserver !== 'undefined' && PerformanceObserver.supportedEntryTypes?.includes('long-animation-frame') };
        case 'layout-shift':
          return { atMs, kind: request.kind, available: typeof PerformanceObserver !== 'undefined' && PerformanceObserver.supportedEntryTypes?.includes('layout-shift') };
        case 'input-to-render':
          return { atMs, kind: request.kind, available: typeof document !== 'undefined' };
        case 'scroll':
          return { atMs, kind: request.kind, available: typeof document !== 'undefined' };
        case 'semantic-dom':
          if (typeof document === 'undefined') return { atMs, kind: request.kind, available: false, error: 'document is unavailable' };
          if (!request.selector) return { atMs, kind: request.kind, available: true, value: { count: document.querySelectorAll('[data-item-id]').length } };
          try {
            const elements = document.querySelectorAll(request.selector);
            const first = elements.item(0);
            const rect = first?.getBoundingClientRect();
            return {
              atMs,
              kind: request.kind,
              available: true,
              value: { count: elements.length, visible: first ? !!rect && rect.width > 0 && rect.height > 0 : false },
            };
          } catch (error) {
            return { atMs, kind: request.kind, available: false, error: `invalid selector: ${String(error)}` };
          }
        case 'focus-clipping-settledness': {
          if (typeof document === 'undefined') return { atMs, kind: request.kind, available: false, error: 'document is unavailable' };
          const active = document.activeElement;
          if (!(active instanceof HTMLElement)) return { atMs, kind: request.kind, available: true, value: { focused: false } };
          const rect = active.getBoundingClientRect();
          return { atMs, kind: request.kind, available: true, value: { focused: true, clipped: rect.bottom < 0 || rect.top > window.innerHeight } };
        }
        case 'source':
        case 'parser': {
          const source = (globalThis as { __aoMonitorSourceSignals?: { read?: unknown } }).__aoMonitorSourceSignals;
          if (typeof source?.read !== 'function') return { atMs, kind: request.kind, available: false, error: 'source adapter is unavailable' };
          try {
            return { atMs, kind: request.kind, available: true, value: source.read() };
          } catch (error) {
            return { atMs, kind: request.kind, available: false, error: String(error) };
          }
        }
        case 'compositor': {
          const compositor = (globalThis as { __aoMonitorCompositorCounters?: { read?: unknown } }).__aoMonitorCompositorCounters;
          if (typeof compositor?.read !== 'function') return { atMs, kind: request.kind, available: false, error: 'compositor adapter is unavailable' };
          try {
            return { atMs, kind: request.kind, available: true, value: compositor.read() };
          } catch (error) {
            return { atMs, kind: request.kind, available: false, error: String(error) };
          }
        }
      }
    },
  };
}

function monitorRunId(spec: Record<string, unknown>): string {
  const runId = str(spec, 'runId');
  if (!runId) throw new Error('monitor operation requires a runId');
  return runId;
}

function monitorIDs(spec: Record<string, unknown>): string[] {
  if (!Array.isArray(spec.monitorIds) || spec.monitorIds.length === 0) {
    throw new Error('monitor start requires a non-empty monitorIds array');
  }
  if (!spec.monitorIds.every((id): id is string => typeof id === 'string')) {
    throw new Error('monitorIds must contain only strings');
  }
  return spec.monitorIds;
}

export function dispatchMonitor(spec: Record<string, unknown>): unknown {
  switch (str(spec, 'op')) {
    case 'list':
      return {
        v: 1,
        monitors: defaultMonitorRegistry.list().map((spec) => {
          const descriptor = { ...spec };
          delete descriptor.create;
          return descriptor;
        }),
      };
    case 'start':
      if (spec.heartbeatTimeoutMs !== undefined && num(spec, 'heartbeatTimeoutMs', 0) <= 0) throw new Error('monitor heartbeatTimeoutMs must be positive');
      if (spec.atMs !== undefined && num(spec, 'atMs', 0) < 0) throw new Error('monitor atMs must be non-negative');
      return defaultMonitorRegistry.start({
        runId: str(spec, 'runId') || undefined,
        monitorIds: monitorIDs(spec),
        capabilities: monitorCapabilities(),
        probes: monitorProbes(),
        heartbeatTimeoutMs: num(spec, 'heartbeatTimeoutMs', 3000),
        atMs: num(spec, 'atMs', typeof performance !== 'undefined' ? performance.now() : Date.now()),
        compatibilityLeg: (str(spec, 'compatibilityLeg') || undefined) as MonitorCompatibilityLeg | undefined,
      });
    case 'heartbeat':
      if (spec.atMs !== undefined && num(spec, 'atMs', 0) < 0) throw new Error('monitor atMs must be non-negative');
      return defaultMonitorRegistry.heartbeat(monitorRunId(spec), num(spec, 'atMs', typeof performance !== 'undefined' ? performance.now() : Date.now()));
    case 'collect':
      if (spec.atMs !== undefined && num(spec, 'atMs', 0) < 0) throw new Error('monitor atMs must be non-negative');
      return defaultMonitorRegistry.collect(monitorRunId(spec), num(spec, 'atMs', typeof performance !== 'undefined' ? performance.now() : Date.now()));
    case 'overlap': {
      const withRunId = str(spec, 'withRunId');
      if (!withRunId) throw new Error('monitor overlap requires a withRunId');
      if (spec.atMs !== undefined && num(spec, 'atMs', 0) < 0) throw new Error('monitor atMs must be non-negative');
      return defaultMonitorRegistry.overlap(monitorRunId(spec), withRunId, num(spec, 'atMs', typeof performance !== 'undefined' ? performance.now() : Date.now()));
    }
    case 'stop':
      if (spec.atMs !== undefined && num(spec, 'atMs', 0) < 0) throw new Error('monitor atMs must be non-negative');
      return defaultMonitorRegistry.stop(monitorRunId(spec), num(spec, 'atMs', typeof performance !== 'undefined' ? performance.now() : Date.now()));
    case 'last':
      return defaultMonitorRegistry.lastStopped(spec.runId === undefined ? undefined : monitorRunId(spec));
    default:
      return fail(`unknown monitor op ${JSON.stringify(str(spec, 'op'))}`);
  }
}

export function stopAllMonitors(atMs?: number) {
  return defaultMonitorRegistry.stopAll(atMs);
}
