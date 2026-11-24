// Validates a functional flow's monitor selection against the built-in
// monitors the page runs (frontend/src/lib/harness/monitorTypes.ts).
import {
  BUILTIN_MONITORS,
  MONITOR_COMPATIBILITY_LEGS,
  type BuiltinMonitorId,
  type MonitorCompatibilityLeg,
  type MonitorDescriptor,
} from '../../frontend/src/lib/harness/monitorTypes.ts';

const byID: ReadonlyMap<string, MonitorDescriptor> = new Map(BUILTIN_MONITORS.map((monitor) => [monitor.id, monitor]));

export function validateMonitorSelection(id: string, compatibilityLeg?: string): BuiltinMonitorId {
  const monitor = byID.get(id);
  if (!monitor) throw new Error(`unknown monitor ${JSON.stringify(id)}`);
  if (compatibilityLeg !== undefined && compatibilityLeg !== monitor.compatibilityLeg) {
    throw new Error(`monitor ${JSON.stringify(id)} belongs to ${monitor.compatibilityLeg}, not ${JSON.stringify(compatibilityLeg)}`);
  }
  return id as BuiltinMonitorId;
}

export function validateCompatibilityLeg(value: string): MonitorCompatibilityLeg {
  if (!MONITOR_COMPATIBILITY_LEGS.includes(value as MonitorCompatibilityLeg)) {
    throw new Error(`unknown compatibility leg ${JSON.stringify(value)} (want ${MONITOR_COMPATIBILITY_LEGS.join(', ')})`);
  }
  return value as MonitorCompatibilityLeg;
}
