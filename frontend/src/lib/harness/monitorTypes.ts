/**
 * Stable types for app-feel monitors.
 *
 * A monitor is an observation contract, not a browser script. The probe
 * surface is deliberately finite, so a custom monitor cannot smuggle
 * arbitrary JavaScript evaluation or screenshots into a harness run.
 */

export const MONITOR_SPEC_VERSION = 1 as const;
export type MonitorSpecVersion = typeof MONITOR_SPEC_VERSION;

export type MonitorId =
  | 'frame-pacing'
  | 'skipped-frames'
  | 'long-task'
  | 'long-animation-frame'
  | 'input-to-render'
  | 'scroll-response'
  | 'layout-shift'
  | 'animation-continuity'
  | 'source-rewind'
  | 'parser-boundary'
  | 'semantic-dom-stability'
  | 'focus-clipping-settledness'
  | 'compositor-counters';

export type MonitorCompatibilityLeg =
  | 'clean-renderer'
  | 'instrumented-renderer'
  | 'functional'
  | 'provider-source'
  | 'platform';

export type MeasurementPerturbation =
  | 'none'
  | 'animation-frame'
  | 'performance-observer'
  | 'event-listener'
  | 'dom-observer'
  | 'layout-read'
  | 'source-instrumentation'
  | 'platform-counter';

export type MonitorCapability =
  | 'animation-frame'
  | 'performance:longtask'
  | 'performance:long-animation-frame'
  | 'performance:event'
  | 'performance:layout-shift'
  | 'dom'
  | 'input'
  | 'scroll'
  | 'semantic-dom'
  | 'focus'
  | 'source-rewind'
  | 'parser-boundary'
  | 'compositor-counters';

export const MONITOR_CAPABILITIES: readonly MonitorCapability[] = [
  'animation-frame',
  'performance:longtask',
  'performance:long-animation-frame',
  'performance:event',
  'performance:layout-shift',
  'dom',
  'input',
  'scroll',
  'semantic-dom',
  'focus',
  'source-rewind',
  'parser-boundary',
  'compositor-counters',
];

export const MONITOR_COMPATIBILITY_LEGS: readonly MonitorCompatibilityLeg[] = [
  'clean-renderer', 'instrumented-renderer', 'functional', 'provider-source', 'platform',
];

export const MONITOR_PERTURBATIONS: readonly MeasurementPerturbation[] = [
  'none', 'animation-frame', 'performance-observer', 'event-listener', 'dom-observer',
  'layout-read', 'source-instrumentation', 'platform-counter',
];

export type MonitorProbeKind =
  | 'frame'
  | 'long-task'
  | 'long-animation-frame'
  | 'input-to-render'
  | 'scroll'
  | 'layout-shift'
  | 'animation'
  | 'source'
  | 'parser'
  | 'semantic-dom'
  | 'focus-clipping-settledness'
  | 'compositor';

export interface MonitorProbeRequest<K extends MonitorProbeKind = MonitorProbeKind> {
  readonly kind: K;
  readonly selector?: string;
}

export interface MonitorProbeResult {
  readonly atMs: number;
  readonly kind: MonitorProbeKind;
  readonly available: boolean;
  readonly value?: unknown;
  readonly error?: string;
}

export interface SourceMonitorSignal {
  readonly cursor: number;
  readonly rewinds?: number;
  readonly parserBoundaries?: number;
}

export interface CompositorMonitorSignal {
  readonly presentedFrames?: number;
  readonly droppedFrames?: number;
  readonly checkerboardFrames?: number;
}

export interface MonitorProbeReader {
  read<K extends MonitorProbeKind>(request: MonitorProbeRequest<K>): MonitorProbeResult;
}

export interface MonitorContext {
  readonly runId: string;
  readonly monitorId: string;
  readonly startedAtMs: number;
  readonly capabilities: ReadonlySet<MonitorCapability>;
  readonly probes: MonitorProbeReader;
  now(): number;
  observe(value: unknown): void;
  fail(message: string): void;
}

export interface MonitorInstance {
  start?(): void;
  heartbeat?(atMs: number): void;
  collect?(atMs: number): unknown;
  overlap?(withRunId: string, atMs: number): void;
  stop?(atMs: number): unknown;
}

export interface MonitorSpec<TInstance extends MonitorInstance = MonitorInstance> {
  readonly v: MonitorSpecVersion;
  readonly id: string;
  readonly title: string;
  readonly description: string;
  readonly compatibilityLeg: MonitorCompatibilityLeg;
  readonly perturbation: MeasurementPerturbation;
  readonly requires: readonly MonitorCapability[];
  readonly create?: (context: MonitorContext) => TInstance;
}

export interface CustomMonitorSpec<TInstance extends MonitorInstance = MonitorInstance>
  extends MonitorSpec<TInstance> {
  readonly custom: true;
}

export interface MonitorStartRequest {
  readonly runId?: string;
  readonly monitorIds: readonly string[];
  readonly capabilities: ReadonlySet<MonitorCapability>;
  readonly heartbeatTimeoutMs?: number;
  readonly atMs?: number;
  /** Typed host probes only. There is no string-eval or screenshot escape hatch. */
  readonly probes?: MonitorProbeReader;
  readonly compatibilityLeg?: MonitorCompatibilityLeg;
}

export interface MonitorOverlap {
  readonly runId: string;
  readonly withRunId: string;
  readonly atMs: number;
}

export interface MonitorObservation {
  readonly atMs: number;
  readonly value: unknown;
}

export interface MonitorResult {
  readonly monitorId: string;
  readonly status: 'complete' | 'partial' | 'failed';
  readonly startedAtMs: number;
  readonly stoppedAtMs: number;
  readonly heartbeats: number;
  readonly lastHeartbeatAtMs: number;
  readonly observations: readonly MonitorObservation[];
  readonly errors: readonly string[];
}

export interface MonitorRunResult {
  readonly v: MonitorSpecVersion;
  readonly runId: string;
  readonly status: 'complete' | 'partial' | 'failed';
  readonly startedAtMs: number;
  readonly stoppedAtMs: number;
  readonly heartbeats: number;
  readonly overlap: readonly MonitorOverlap[];
  readonly monitors: readonly MonitorResult[];
  readonly errors: readonly string[];
}

export interface MonitorStartReceipt {
  readonly v: MonitorSpecVersion;
  readonly runId: string;
  readonly startedAtMs: number;
  readonly monitors: readonly MonitorSpec[];
  readonly overlap: readonly MonitorOverlap[];
}

export interface MonitorCollectReceipt {
  readonly v: MonitorSpecVersion;
  readonly runId: string;
  readonly atMs: number;
  readonly monitors: readonly MonitorResult[];
  readonly partial: boolean;
}

export interface MonitorLastReceipt {
  readonly v: MonitorSpecVersion;
  readonly runs: readonly MonitorRunResult[];
}

export class MonitorRefusal extends Error {
  readonly code = 'capability_refused' as const;
  readonly monitorId: string;
  readonly missing: readonly MonitorCapability[];

  constructor(monitorId: string, missing: readonly MonitorCapability[]) {
    super(`monitor ${JSON.stringify(monitorId)} requires unavailable capabilities: ${missing.join(', ')}`);
    this.name = 'MonitorRefusal';
    this.monitorId = monitorId;
    this.missing = [...missing];
  }
}

export class MonitorRunError extends Error {
  constructor(message: string) {
    super(message);
    this.name = 'MonitorRunError';
  }
}
