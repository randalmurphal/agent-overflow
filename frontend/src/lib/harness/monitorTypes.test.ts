import { expect, it } from 'vitest';
import harnessMonitor from '../../../../cmd/ao-harness/cmd_monitor.go?raw';
import { BUILTIN_MONITORS, MONITOR_COMPATIBILITY_LEGS } from './monitorTypes';

// ao-harness refuses a monitor leg before it reaches a page, so its list
// of legs must be the page's.
it('ao-harness accepts the same monitor compatibility legs the page has', () => {
  const declared = /var monitorCompatibilityLegs = \[\]string\{([^}]*)\}/.exec(harnessMonitor);
  expect(declared, 'cmd/ao-harness/cmd_monitor.go declares monitorCompatibilityLegs').not.toBeNull();
  const goLegs = [...declared![1]!.matchAll(/"([^"]+)"/g)].map((match) => match[1]);
  expect(goLegs).toEqual([...MONITOR_COMPATIBILITY_LEGS]);
});

// A leg no built-in monitor belongs to selects nothing.
it('every compatibility leg has a built-in monitor', () => {
  expect(new Set(BUILTIN_MONITORS.map((monitor) => monitor.compatibilityLeg))).toEqual(new Set(MONITOR_COMPATIBILITY_LEGS));
});
