export const MONITOR_SPEC_VERSION = 1 as const;

export type MonitorId =
  | 'frame-pacing' | 'skipped-frames' | 'long-task' | 'long-animation-frame'
  | 'input-to-render' | 'scroll-response' | 'layout-shift' | 'animation-continuity'
  | 'source-rewind' | 'parser-boundary' | 'semantic-dom-stability'
  | 'focus-clipping-settledness' | 'compositor-counters';

export type CompatibilityLeg = 'clean-renderer' | 'instrumented-renderer' | 'functional' | 'provider-source' | 'platform';
export const COMPATIBILITY_LEGS: readonly CompatibilityLeg[] = ['clean-renderer', 'instrumented-renderer', 'functional', 'provider-source', 'platform'];
export type Perturbation = 'none' | 'animation-frame' | 'performance-observer' | 'event-listener' | 'dom-observer' | 'layout-read' | 'source-instrumentation' | 'platform-counter';

export interface MonitorSpec {
  readonly v: typeof MONITOR_SPEC_VERSION;
  readonly id: MonitorId;
  readonly title: string;
  readonly compatibilityLeg: CompatibilityLeg;
  readonly perturbation: Perturbation;
  readonly requires: readonly string[];
}

const define = (id: MonitorId, title: string, compatibilityLeg: CompatibilityLeg, perturbation: Perturbation, ...requires: string[]): MonitorSpec => ({ v: 1, id, title, compatibilityLeg, perturbation, requires });

export const MONITOR_CATALOG: readonly MonitorSpec[] = Object.freeze([
  define('frame-pacing', 'Frame pacing', 'clean-renderer', 'animation-frame', 'animation-frame'),
  define('skipped-frames', 'Skipped frames', 'clean-renderer', 'animation-frame', 'animation-frame'),
  define('long-task', 'Long tasks', 'instrumented-renderer', 'performance-observer', 'performance:longtask'),
  define('long-animation-frame', 'Long animation frames', 'instrumented-renderer', 'performance-observer', 'performance:long-animation-frame'),
  define('input-to-render', 'Input to render', 'instrumented-renderer', 'event-listener', 'input', 'animation-frame'),
  define('scroll-response', 'Scroll response', 'instrumented-renderer', 'event-listener', 'scroll', 'animation-frame'),
  define('layout-shift', 'Layout shift', 'instrumented-renderer', 'performance-observer', 'performance:layout-shift'),
  define('animation-continuity', 'Animation continuity', 'instrumented-renderer', 'animation-frame', 'animation-frame'),
  define('source-rewind', 'Source rewind', 'provider-source', 'source-instrumentation', 'source-rewind'),
  define('parser-boundary', 'Parser boundary', 'provider-source', 'source-instrumentation', 'parser-boundary'),
  define('semantic-dom-stability', 'Semantic DOM stability', 'functional', 'dom-observer', 'dom', 'semantic-dom'),
  define('focus-clipping-settledness', 'Focus, clipping and settledness', 'functional', 'layout-read', 'dom', 'focus'),
  define('compositor-counters', 'Compositor counters', 'platform', 'platform-counter', 'compositor-counters'),
]);

const byID = new Map(MONITOR_CATALOG.map((spec) => [spec.id, spec]));

export function monitorSpec(id: string): MonitorSpec | undefined {
  return byID.get(id as MonitorId);
}

export function validateMonitorSelection(id: string, compatibilityLeg?: string): MonitorSpec {
  const spec = monitorSpec(id);
  if (!spec) throw new Error(`unknown monitor ${JSON.stringify(id)}`);
  if (compatibilityLeg !== undefined && compatibilityLeg !== spec.compatibilityLeg) {
    throw new Error(`monitor ${JSON.stringify(id)} belongs to ${spec.compatibilityLeg}, not ${JSON.stringify(compatibilityLeg)}`);
  }
  return spec;
}

export function validateCompatibilityLeg(value: string): CompatibilityLeg {
  if (!COMPATIBILITY_LEGS.includes(value as CompatibilityLeg)) throw new Error(`unknown compatibility leg ${JSON.stringify(value)}`);
  return value as CompatibilityLeg;
}
