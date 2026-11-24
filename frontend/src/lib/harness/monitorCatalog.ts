import { createBuiltinMonitor } from './monitorHosts';
import { BUILTIN_MONITORS, MONITOR_SPEC_VERSION, type MonitorSpec } from './monitorTypes';

/** The built-in monitors, each with its browser host. */
export function builtinMonitorSpecs(): readonly MonitorSpec[] {
  return BUILTIN_MONITORS.map((descriptor) => {
    const spec: MonitorSpec = { v: MONITOR_SPEC_VERSION, ...descriptor };
    return { ...spec, create: (context) => createBuiltinMonitor(spec, context)! };
  });
}
