import { defaultMonitorRegistry } from './monitorRegistry';
import type { MonitorCapability, MonitorCompatibilityLeg } from './monitorTypes';

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
  return capabilities;
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
