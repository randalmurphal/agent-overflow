import {
  MONITOR_SPEC_VERSION,
  MONITOR_CAPABILITIES,
  MONITOR_COMPATIBILITY_LEGS,
  MONITOR_PERTURBATIONS,
  MonitorRefusal,
  MonitorRunError,
  type CustomMonitorSpec,
  type MonitorCapability,
  type MonitorCollectReceipt,
  type MonitorContext,
  type MonitorInstance,
  type MonitorLastReceipt,
  type MonitorOverlap,
  type MonitorProbeReader,
  type MonitorResult,
  type MonitorRunResult,
  type MonitorSpec,
  type MonitorStartReceipt,
  type MonitorStartRequest,
} from './monitorTypes';
import { builtinMonitorSpecs } from './monitorCatalog';

const DEFAULT_HEARTBEAT_TIMEOUT_MS = 3_000;
const MAX_MONITOR_OBSERVATIONS = 256;
const MAX_MONITOR_OBSERVATION_DEPTH = 8;
const MAX_MONITOR_OBSERVATION_KEYS = 128;
const MAX_MONITOR_OBSERVATION_STRING = 16 * 1024;
const MAX_MONITOR_OBSERVATION_CHARS = 64 * 1024;
const MAX_STOPPED_RUNS = 32;

interface LiveMonitor {
  spec: MonitorSpec;
  instance: MonitorInstance;
  startedAtMs: number;
  lastHeartbeatAtMs: number;
  heartbeats: number;
  observations: Array<{ atMs: number; value: unknown }>;
  observationDrops: number;
  errors: string[];
}

interface LiveRun {
  id: string;
  startedAtMs: number;
  lastHeartbeatAtMs: number;
  heartbeats: number;
  heartbeatTimeoutMs: number;
  overlaps: MonitorOverlap[];
  errors: string[];
  monitors: LiveMonitor[];
}

/**
 * A registry is the only construction path for a monitor run. It validates
 * versions, capabilities and IDs before touching a browser observer. Runs
 * may overlap, but the overlap is explicit evidence on every affected run.
 */
export class MonitorRegistry {
  private readonly specs = new Map<string, MonitorSpec>();
  private readonly runs = new Map<string, LiveRun>();
  private readonly stoppedRuns: MonitorRunResult[] = [];
  private nextRun = 1;

  constructor(specs: readonly MonitorSpec[] = builtinMonitorSpecs()) {
    for (const spec of specs) this.register(spec);
  }

  register<TInstance extends MonitorInstance>(spec: CustomMonitorSpec<TInstance>): void;
  register<TInstance extends MonitorInstance>(spec: MonitorSpec<TInstance>): void;
  register(spec: MonitorSpec): void {
    if (spec.v !== MONITOR_SPEC_VERSION) {
      throw new MonitorRunError(`monitor ${JSON.stringify(spec.id)} has unsupported spec version ${spec.v}`);
    }
    if (!spec.id || !/^[a-z][a-z0-9-]*$/.test(spec.id)) {
      throw new MonitorRunError(`monitor ID ${JSON.stringify(spec.id)} is not a stable kebab-case name`);
    }
    if (!spec.title || !spec.description) throw new MonitorRunError(`monitor ${JSON.stringify(spec.id)} needs a title and description`);
    if (!MONITOR_COMPATIBILITY_LEGS.includes(spec.compatibilityLeg)) {
      throw new MonitorRunError(`monitor ${JSON.stringify(spec.id)} has an unknown compatibility leg`);
    }
    if (!MONITOR_PERTURBATIONS.includes(spec.perturbation)) {
      throw new MonitorRunError(`monitor ${JSON.stringify(spec.id)} has an unknown measurement perturbation`);
    }
    const requirements = new Set(spec.requires);
    if (requirements.size !== spec.requires.length || spec.requires.some((capability) => !MONITOR_CAPABILITIES.includes(capability))) {
      throw new MonitorRunError(`monitor ${JSON.stringify(spec.id)} has invalid or duplicate capability requirements`);
    }
    if ('custom' in spec && spec.custom && typeof spec.create !== 'function') {
      throw new MonitorRunError(`custom monitor ${JSON.stringify(spec.id)} must provide a typed factory`);
    }
    if (this.specs.has(spec.id)) {
      throw new MonitorRunError(`monitor ${JSON.stringify(spec.id)} is already registered`);
    }
    this.specs.set(spec.id, freezeSpec(spec));
  }

  list(): readonly MonitorSpec[] {
    return [...this.specs.values()].sort((a, b) => a.id.localeCompare(b.id));
  }

  get(id: string): MonitorSpec | undefined {
    return this.specs.get(id);
  }

  activeRunIds(): readonly string[] {
    return [...this.runs.keys()].sort();
  }

  start(request: MonitorStartRequest): MonitorStartReceipt {
    const ids = [...request.monitorIds];
    if (ids.length === 0) throw new MonitorRunError('monitor run requires at least one monitor ID');
    const capabilities = new Set(request.capabilities);
    const seen = new Set<string>();
    const selected: MonitorSpec[] = [];
    for (const id of ids) {
      if (seen.has(id)) throw new MonitorRunError(`monitor ${JSON.stringify(id)} is listed more than once`);
      seen.add(id);
      const spec = this.specs.get(id);
      if (!spec) throw new MonitorRunError(`unknown monitor ${JSON.stringify(id)} (use monitor list)`);
      if (request.compatibilityLeg !== undefined && spec.compatibilityLeg !== request.compatibilityLeg) {
        throw new MonitorRunError(`monitor ${JSON.stringify(id)} belongs to ${spec.compatibilityLeg}, not ${JSON.stringify(request.compatibilityLeg)}`);
      }
      const missing = spec.requires.filter((capability) => !capabilities.has(capability));
      if (missing.length > 0) throw new MonitorRefusal(id, missing);
      selected.push(spec);
    }

    const atMs = finiteTime(request.atMs, 'start timestamp');
    const requestedId = request.runId?.trim();
    let id = requestedId || `monitor-${this.nextRun++}`;
    while (!requestedId && this.runs.has(id)) id = `monitor-${this.nextRun++}`;
    if (this.runs.has(id)) throw new MonitorRunError(`monitor run ${JSON.stringify(id)} is already active`);
    const overlap: MonitorOverlap[] = [];
    const existingOverlapLengths = [...this.runs.values()].map((existing) => ({ run: existing, length: existing.overlaps.length }));
    for (const existing of this.runs.values()) {
      const event = { runId: id, withRunId: existing.id, atMs } satisfies MonitorOverlap;
      overlap.push(event);
      existing.overlaps.push({ runId: existing.id, withRunId: id, atMs });
    }

    const run: LiveRun = {
      id,
      startedAtMs: atMs,
      lastHeartbeatAtMs: atMs,
      heartbeats: 0,
      heartbeatTimeoutMs: positiveOrDefault(request.heartbeatTimeoutMs, DEFAULT_HEARTBEAT_TIMEOUT_MS),
      overlaps: overlap,
      errors: [],
      monitors: [],
    };
    this.runs.set(id, run);
    try {
      for (const spec of selected) {
        if (!spec.create) {
          throw new MonitorRunError(
            `monitor ${JSON.stringify(spec.id)} is declared but has no typed host implementation`,
          );
        }
        const monitor = this.createMonitor(run, spec, capabilities, request.probes);
        run.monitors.push(monitor);
        monitor.instance.start?.();
      }
    } catch (error) {
      const cleanupErrors: string[] = [];
      for (const monitor of [...run.monitors].reverse()) {
        try {
          monitor.instance.stop?.(atMs);
        } catch (cleanupError) {
          cleanupErrors.push(`${monitor.spec.id}: ${errorMessage(cleanupError)}`);
        }
      }
      for (const { run: existing, length } of existingOverlapLengths) existing.overlaps.length = length;
      this.runs.delete(id);
      const suffix = cleanupErrors.length > 0 ? `; cleanup failed: ${cleanupErrors.join('; ')}` : '';
      throw new MonitorRunError(`start monitor run ${JSON.stringify(id)}: ${errorMessage(error)}${suffix}`);
    }
    return {
      v: MONITOR_SPEC_VERSION,
      runId: id,
      startedAtMs: atMs,
      monitors: selected,
      overlap,
    };
  }

  heartbeat(runId: string, atMs?: number): { v: 1; runId: string; atMs: number; heartbeats: number; partial: boolean } {
    const run = this.requireRun(runId);
    const at = finiteTime(atMs, 'heartbeat timestamp');
    if (at < run.lastHeartbeatAtMs) throw new MonitorRunError(`heartbeat for run ${JSON.stringify(runId)} moved backwards`);
    run.lastHeartbeatAtMs = at;
    run.heartbeats += 1;
    for (const monitor of run.monitors) {
      monitor.lastHeartbeatAtMs = at;
      monitor.heartbeats += 1;
      try {
        monitor.instance.heartbeat?.(at);
      } catch (error) {
        monitor.errors.push(`heartbeat: ${errorMessage(error)}`);
      }
    }
    return {
      v: MONITOR_SPEC_VERSION,
      runId,
      atMs: at,
      heartbeats: run.heartbeats,
      partial: run.errors.length > 0 || run.monitors.some((monitor) => monitor.errors.length > 0),
    };
  }

  collect(runId: string, atMs?: number): MonitorCollectReceipt {
    const run = this.requireRun(runId);
    const at = finiteTime(atMs, 'collect timestamp');
    if (at < run.startedAtMs) throw new MonitorRunError(`collect for run ${JSON.stringify(runId)} moved before its start`);
    for (const monitor of run.monitors) {
      try {
        const value = monitor.instance.collect?.(at);
        if (value !== undefined) this.recordObservation(monitor, at, value, 'collect');
      } catch (error) {
        monitor.errors.push(`collect: ${errorMessage(error)}`);
      }
    }
    const monitors = run.monitors.map((monitor) => this.currentResult(monitor, at));
    return {
      v: MONITOR_SPEC_VERSION,
      runId,
      atMs: at,
      monitors,
      partial: run.errors.length > 0 || monitors.some((monitor) => monitor.status !== 'complete'),
    };
  }

  overlap(runId: string, withRunId: string, atMs?: number): MonitorOverlap {
    const run = this.requireRun(runId);
    const other = this.requireRun(withRunId);
    if (run.id === other.id) throw new MonitorRunError('a monitor run cannot overlap itself');
    const at = finiteTime(atMs, 'overlap timestamp');
    if (at < run.startedAtMs || at < other.startedAtMs) {
      throw new MonitorRunError('overlap timestamp precedes one of the monitor run starts');
    }
    const event = { runId, withRunId, atMs: at } satisfies MonitorOverlap;
    if (!run.overlaps.some((item) => item.withRunId === withRunId && item.atMs === at)) run.overlaps.push(event);
    if (!other.overlaps.some((item) => item.withRunId === runId && item.atMs === at)) {
      other.overlaps.push({ runId: withRunId, withRunId: runId, atMs: at });
    }
    const callbackErrors: string[] = [];
    for (const monitor of run.monitors) {
      try {
        monitor.instance.overlap?.(withRunId, at);
      } catch (error) {
        const message = `overlap: ${errorMessage(error)}`;
        monitor.errors.push(message);
        callbackErrors.push(`${runId}/${monitor.spec.id}: ${message}`);
      }
    }
    for (const monitor of other.monitors) {
      try {
        monitor.instance.overlap?.(runId, at);
      } catch (error) {
        const message = `overlap: ${errorMessage(error)}`;
        monitor.errors.push(message);
        callbackErrors.push(`${withRunId}/${monitor.spec.id}: ${message}`);
      }
    }
    if (callbackErrors.length > 0) throw new MonitorRunError(`monitor overlap callbacks failed: ${callbackErrors.join('; ')}`);
    return event;
  }

  stop(runId: string, atMs?: number): MonitorRunResult {
    const run = this.requireRun(runId);
    const at = finiteTime(atMs, 'stop timestamp');
    if (at < run.startedAtMs) throw new MonitorRunError(`stop for run ${JSON.stringify(runId)} moved before its start`);
    this.runs.delete(runId);
    if (at - run.lastHeartbeatAtMs > run.heartbeatTimeoutMs) {
      run.errors.push(`heartbeat gap ${Math.round(at - run.lastHeartbeatAtMs)}ms exceeded ${run.heartbeatTimeoutMs}ms`);
    }
    const monitors = run.monitors.map((monitor) => this.stopMonitor(monitor, at));
    const failed = run.errors.length > 0 || monitors.some((monitor) => monitor.status !== 'complete');
    const result: MonitorRunResult = {
      v: MONITOR_SPEC_VERSION,
      runId,
      status: failed ? 'partial' : 'complete',
      startedAtMs: run.startedAtMs,
      stoppedAtMs: at,
      heartbeats: run.heartbeats,
      overlap: run.overlaps,
      monitors,
      errors: run.errors,
    };
    this.stoppedRuns.push(result);
    if (this.stoppedRuns.length > MAX_STOPPED_RUNS) this.stoppedRuns.shift();
    return result;
  }

  stopAll(atMs?: number): readonly MonitorRunResult[] {
    const at = finiteTime(atMs, 'stop timestamp');
    return this.activeRunIds().map((runId) => this.stop(runId, at));
  }

  lastStopped(runId?: string): MonitorLastReceipt {
    const runs = runId === undefined
      ? this.stoppedRuns
      : this.stoppedRuns.filter((run) => run.runId === runId);
    return { v: MONITOR_SPEC_VERSION, runs: [...runs] };
  }

  private createMonitor(
    run: LiveRun,
    spec: MonitorSpec,
    capabilities: ReadonlySet<MonitorCapability>,
    probes?: MonitorProbeReader,
  ): LiveMonitor {
    const monitor: LiveMonitor = {
      spec,
      instance: {},
      startedAtMs: run.startedAtMs,
      lastHeartbeatAtMs: run.startedAtMs,
      heartbeats: 0,
      observations: [],
      observationDrops: 0,
      errors: [],
    };
    const context: MonitorContext = {
      runId: run.id,
      monitorId: spec.id,
      startedAtMs: run.startedAtMs,
      capabilities,
      probes: probes ?? { read: () => ({ atMs: run.lastHeartbeatAtMs, kind: 'frame', available: false, error: 'no probe supplied' }) },
      now: () => run.lastHeartbeatAtMs,
      observe: (value) => {
        this.recordObservation(monitor, run.lastHeartbeatAtMs, value, 'observe');
      },
      fail: (message) => monitor.errors.push(message || 'monitor reported an unspecified failure'),
    };
    monitor.instance = spec.create?.(context) ?? {};
    return monitor;
  }

  private stopMonitor(monitor: LiveMonitor, atMs: number): MonitorResult {
    try {
      const value = monitor.instance.stop?.(atMs);
      if (value !== undefined) this.recordObservation(monitor, atMs, value, 'stop');
    } catch (error) {
      monitor.errors.push(`stop: ${errorMessage(error)}`);
    }
    const errors = monitor.observationDrops > 0
      ? [...monitor.errors, `observation limit reached; dropped ${monitor.observationDrops} samples`]
      : monitor.errors;
    return {
      monitorId: monitor.spec.id,
      status: errors.length > 0 ? 'partial' : 'complete',
      startedAtMs: monitor.startedAtMs,
      stoppedAtMs: atMs,
      heartbeats: monitor.heartbeats,
      lastHeartbeatAtMs: monitor.lastHeartbeatAtMs,
      observations: monitor.observations,
      errors,
    };
  }

  private currentResult(monitor: LiveMonitor, atMs: number): MonitorResult {
    const errors = monitor.observationDrops > 0
      ? [...monitor.errors, `observation limit reached; dropped ${monitor.observationDrops} samples`]
      : monitor.errors;
    return {
      monitorId: monitor.spec.id,
      status: errors.length > 0 ? 'partial' : 'complete',
      startedAtMs: monitor.startedAtMs,
      stoppedAtMs: atMs,
      heartbeats: monitor.heartbeats,
      lastHeartbeatAtMs: monitor.lastHeartbeatAtMs,
      observations: [...monitor.observations],
      errors: [...errors],
    };
  }

  private recordObservation(monitor: LiveMonitor, atMs: number, value: unknown, source: string): void {
    if (monitor.observations.length >= MAX_MONITOR_OBSERVATIONS) {
      monitor.observationDrops += 1;
      return;
    }
    try {
      const bounded = boundObservation(value);
      monitor.observations.push({ atMs, value: bounded });
    } catch (error) {
      monitor.errors.push(`${source} observation rejected: ${errorMessage(error)}`);
    }
  }

  private requireRun(runId: string): LiveRun {
    const run = this.runs.get(runId);
    if (!run) throw new MonitorRunError(`monitor run ${JSON.stringify(runId)} is not active`);
    return run;
  }
}

function boundObservation(value: unknown): unknown {
  const state = { chars: 0, objects: new Set<object>() };
  const bounded = boundObservationValue(value, state, 0);
  if (state.chars > MAX_MONITOR_OBSERVATION_CHARS) {
    throw new Error(`observation exceeds ${MAX_MONITOR_OBSERVATION_CHARS} characters`);
  }
  return bounded;
}

function boundObservationValue(value: unknown, state: { chars: number; objects: Set<object> }, depth: number): unknown {
  if (value === null || typeof value === 'boolean') return value;
  if (typeof value === 'number') {
    if (!Number.isFinite(value)) throw new Error('observation contains a non-finite number');
    state.chars += 16;
    return value;
  }
  if (typeof value === 'string') {
    if (value.length > MAX_MONITOR_OBSERVATION_STRING) throw new Error(`string exceeds ${MAX_MONITOR_OBSERVATION_STRING} characters`);
    state.chars += value.length;
    if (state.chars > MAX_MONITOR_OBSERVATION_CHARS) throw new Error(`observation exceeds ${MAX_MONITOR_OBSERVATION_CHARS} characters`);
    return value;
  }
  if (typeof value !== 'object') throw new Error(`unsupported observation value type ${typeof value}`);
  if (depth >= MAX_MONITOR_OBSERVATION_DEPTH) throw new Error(`observation exceeds depth ${MAX_MONITOR_OBSERVATION_DEPTH}`);
  if (state.objects.has(value)) throw new Error('observation contains a cycle');
  state.objects.add(value);
  try {
    if (Array.isArray(value)) {
      if (value.length > MAX_MONITOR_OBSERVATION_KEYS) throw new Error(`array exceeds ${MAX_MONITOR_OBSERVATION_KEYS} entries`);
      return value.map((item) => boundObservationValue(item, state, depth + 1));
    }
    const keys = Object.keys(value);
    if (keys.length > MAX_MONITOR_OBSERVATION_KEYS) throw new Error(`object exceeds ${MAX_MONITOR_OBSERVATION_KEYS} fields`);
    const result: Record<string, unknown> = {};
    for (const key of keys) {
      if (key.length > MAX_MONITOR_OBSERVATION_STRING) throw new Error('observation field name is too long');
      state.chars += key.length;
      result[key] = boundObservationValue((value as Record<string, unknown>)[key], state, depth + 1);
    }
    return result;
  } finally {
    state.objects.delete(value);
  }
}

function finiteTime(value: number | undefined, label: string): number {
  const result = value ?? (typeof performance !== 'undefined' ? performance.now() : Date.now());
  if (!Number.isFinite(result) || result < 0) throw new MonitorRunError(`${label} must be a finite non-negative number`);
  return result;
}

function positiveOrDefault(value: number | undefined, fallback: number): number {
  return value !== undefined && Number.isFinite(value) && value > 0 ? value : fallback;
}

function errorMessage(error: unknown): string {
  return error instanceof Error ? error.message : String(error);
}

function freezeSpec(spec: MonitorSpec): MonitorSpec {
  return Object.freeze({ ...spec, requires: Object.freeze([...spec.requires]) });
}

export const defaultMonitorRegistry = new MonitorRegistry();

export function monitorSpecs(): readonly MonitorSpec[] {
  return defaultMonitorRegistry.list();
}

/** Registers application-owned code through the typed monitor contract. */
export function registerCustomMonitor<TInstance extends MonitorInstance>(
  spec: CustomMonitorSpec<TInstance>,
): void {
  defaultMonitorRegistry.register(spec);
}
