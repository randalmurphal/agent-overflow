import { createBuiltinMonitor } from './monitorHosts';
import type { MonitorCapability, MonitorSpec } from './monitorTypes';

/** The built-in monitor contracts owned by the frontend harness. */
export function builtinMonitorSpecs(): readonly MonitorSpec[] {
  return [
    browserBuiltin('frame-pacing', 'Frame pacing', 'Measures frame callback cadence.', 'clean-renderer', 'animation-frame', ['animation-frame']),
    browserBuiltin('skipped-frames', 'Skipped frames', 'Counts missed frame opportunities.', 'clean-renderer', 'animation-frame', ['animation-frame']),
    browserBuiltin('long-task', 'Long tasks', 'Records main-thread long tasks.', 'instrumented-renderer', 'performance-observer', ['performance:longtask']),
    browserBuiltin('long-animation-frame', 'Long animation frames', 'Records long animation frame entries.', 'instrumented-renderer', 'performance-observer', ['performance:long-animation-frame']),
    browserBuiltin('input-to-render', 'Input to render', 'Measures input delivery through the next render.', 'instrumented-renderer', 'event-listener', ['input', 'animation-frame']),
    browserBuiltin('scroll-response', 'Scroll response', 'Measures scroll input and visual response.', 'instrumented-renderer', 'event-listener', ['scroll', 'animation-frame']),
    browserBuiltin('layout-shift', 'Layout shift', 'Records cumulative layout shift entries.', 'instrumented-renderer', 'performance-observer', ['performance:layout-shift']),
    browserBuiltin('animation-continuity', 'Animation continuity', 'Checks animation progress across frames.', 'instrumented-renderer', 'animation-frame', ['animation-frame']),
    sourceBuiltin('source-rewind', 'Source rewind', 'Checks source progress and rewind boundaries.', ['source-rewind']),
    sourceBuiltin('parser-boundary', 'Parser boundary', 'Checks parser progress at chunk boundaries.', ['parser-boundary']),
    browserBuiltin('semantic-dom-stability', 'Semantic DOM stability', 'Checks semantic row identity and mount stability.', 'functional', 'dom-observer', ['dom', 'semantic-dom']),
    browserBuiltin('focus-clipping-settledness', 'Focus, clipping and settledness', 'Checks focus ownership, clipping and post-mutation settling.', 'functional', 'layout-read', ['dom', 'focus']),
    platformBuiltin('compositor-counters', 'Compositor counters', 'Reads compositor counters when the platform exposes them.', ['compositor-counters']),
  ];
}

function browserBuiltin(
  id: string,
  title: string,
  description: string,
  compatibilityLeg: 'clean-renderer' | 'instrumented-renderer' | 'functional',
  perturbation: 'animation-frame' | 'performance-observer' | 'event-listener' | 'dom-observer' | 'layout-read',
  requires: readonly MonitorCapability[],
): MonitorSpec {
  const spec = { v: 1 as const, id, title, description, compatibilityLeg, perturbation, requires };
  return { ...spec, create: (context) => createBuiltinMonitor(spec, context)! };
}

function sourceBuiltin(id: string, title: string, description: string, requires: readonly MonitorCapability[]): MonitorSpec {
  const spec = { v: 1 as const, id, title, description, compatibilityLeg: 'provider-source' as const, perturbation: 'source-instrumentation' as const, requires };
  return { ...spec, create: (context) => createBuiltinMonitor(spec, context)! };
}

function platformBuiltin(id: string, title: string, description: string, requires: readonly MonitorCapability[]): MonitorSpec {
  const spec = { v: 1 as const, id, title, description, compatibilityLeg: 'platform' as const, perturbation: 'platform-counter' as const, requires };
  return { ...spec, create: (context) => createBuiltinMonitor(spec, context)! };
}
