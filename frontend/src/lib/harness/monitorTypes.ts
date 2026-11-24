/**
 * Stable types for app-feel monitors.
 *
 * A monitor is an observation contract, not a browser script. Its context
 * carries the page's capabilities, the run clock, and `observe` and `fail`,
 * so a custom monitor cannot smuggle arbitrary JavaScript evaluation or
 * screenshots into a harness run.
 */

export const MONITOR_SPEC_VERSION = 1 as const;
export type MonitorSpecVersion = typeof MONITOR_SPEC_VERSION;

export type MonitorCompatibilityLeg =
  | 'clean-renderer'
  | 'instrumented-renderer'
  | 'functional';

export type MeasurementPerturbation =
  | 'none'
  | 'animation-frame'
  | 'performance-observer'
  | 'event-listener'
  | 'dom-observer'
  | 'layout-read';

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
  | 'focus';

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
];

export const MONITOR_COMPATIBILITY_LEGS: readonly MonitorCompatibilityLeg[] = [
  'clean-renderer', 'instrumented-renderer', 'functional',
];

export const MONITOR_PERTURBATIONS: readonly MeasurementPerturbation[] = [
  'none', 'animation-frame', 'performance-observer', 'event-listener', 'dom-observer',
  'layout-read',
];

/** A monitor's contract without its host: what `monitor list` reports. */
export type MonitorDescriptor = Omit<MonitorSpec, 'v' | 'create'>;

/**
 * The built-in monitors, the one list of them: the frontend catalog
 * (monitorCatalog.ts) gives each its browser host, and the e2e functional
 * flow (e2e/src/monitor-catalog.ts) validates a scenario's monitors
 * against it. This module imports nothing, so e2e can load it.
 */
export const BUILTIN_MONITORS = [
  { id: 'frame-pacing', title: 'Frame pacing', description: 'Measures frame callback cadence.', compatibilityLeg: 'clean-renderer', perturbation: 'animation-frame', requires: ['animation-frame'] },
  { id: 'skipped-frames', title: 'Skipped frames', description: 'Counts missed frame opportunities.', compatibilityLeg: 'clean-renderer', perturbation: 'animation-frame', requires: ['animation-frame'] },
  { id: 'long-task', title: 'Long tasks', description: 'Records main-thread long tasks.', compatibilityLeg: 'instrumented-renderer', perturbation: 'performance-observer', requires: ['performance:longtask'] },
  { id: 'long-animation-frame', title: 'Long animation frames', description: 'Records long animation frame entries.', compatibilityLeg: 'instrumented-renderer', perturbation: 'performance-observer', requires: ['performance:long-animation-frame'] },
  { id: 'input-to-render', title: 'Input to render', description: 'Measures input delivery through the next render.', compatibilityLeg: 'instrumented-renderer', perturbation: 'event-listener', requires: ['input', 'animation-frame'] },
  { id: 'scroll-response', title: 'Scroll response', description: 'Measures scroll input and visual response.', compatibilityLeg: 'instrumented-renderer', perturbation: 'event-listener', requires: ['scroll', 'animation-frame'] },
  { id: 'layout-shift', title: 'Layout shift', description: 'Records cumulative layout shift entries.', compatibilityLeg: 'instrumented-renderer', perturbation: 'performance-observer', requires: ['performance:layout-shift'] },
  { id: 'animation-continuity', title: 'Animation continuity', description: 'Checks animation progress across frames.', compatibilityLeg: 'instrumented-renderer', perturbation: 'animation-frame', requires: ['animation-frame'] },
  { id: 'semantic-dom-stability', title: 'Semantic DOM stability', description: 'Checks semantic row identity and mount stability.', compatibilityLeg: 'functional', perturbation: 'dom-observer', requires: ['dom', 'semantic-dom'] },
  { id: 'focus-clipping-settledness', title: 'Focus, clipping and settledness', description: 'Checks focus ownership, clipping and post-mutation settling.', compatibilityLeg: 'functional', perturbation: 'layout-read', requires: ['dom', 'focus'] },
] as const satisfies readonly MonitorDescriptor[];

/** @public The Playwright suite reads it (e2e/src/flow-model.ts). */
export type BuiltinMonitorId = (typeof BUILTIN_MONITORS)[number]['id'];

export interface MonitorContext {
  readonly runId: string;
  readonly monitorId: string;
  readonly startedAtMs: number;
  readonly capabilities: ReadonlySet<MonitorCapability>;
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
