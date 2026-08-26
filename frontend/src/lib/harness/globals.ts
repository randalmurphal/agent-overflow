// Whitelisted reads of the diagnostic globals the app already installs
// (§4 of docs/specs/testing-harness.md, the `globals` query kind).
//
// Two rules, and they pull in opposite directions on purpose:
//
//   * An UNKNOWN name is an ERROR. The whitelist is the security surface
//     of this query — without it `globals` is `eval` with extra steps —
//     and a typo silently answering "unavailable" would make the
//     whitelist untestable from the outside.
//
//   * A known-but-ABSENT global is a normal answer, `{unavailable:true}`.
//     Absence is the common case, not a fault: `__paneGeometry` and
//     `__agentOverflowUiTrace` only exist in a UI_TRACE build, and
//     `make harness` builds with UI_TRACE unset (`UI_TRACE ?= $(DEBUG)`
//     in the Makefile). A caller asking a harness build for pane geometry
//     is asking a reasonable question whose answer is "not in this
//     build"; erroring would make it indistinguishable from asking for a
//     name that does not exist at all.
//
// Every entry is a READ. Nothing here enables, clears, flushes or
// otherwise changes what it is reporting on.

export interface GlobalsResult {
  v: 1;
  name: string;
  unavailable?: true;
  value?: unknown;
}

interface HarnessWindow {
  __aoMemoryReport?: () => Promise<unknown>;
  __aoRevealDrain?: () => Promise<unknown>;
  __paneGeometry?: () => unknown;
  __agentOverflowTimelineMemoryStats?: () => unknown;
  __agentOverflowTimelineMemoryStatsByPane?: () => unknown;
  __stickState?: () => unknown;
  __agentOverflowUiTrace?: { recent?: (count?: number) => unknown };
}

/** Reader for one whitelisted name; `undefined` means "not in this build". */
type GlobalReader = (win: HarnessWindow, args: readonly unknown[]) => unknown;

function numArg(args: readonly unknown[], index: number, fallback: number): number {
  const raw = args[index];
  return typeof raw === 'number' && Number.isFinite(raw) ? raw : fallback;
}

const READERS: Record<string, GlobalReader> = {
  // Async by construction: main.ts installs it as a stub that dynamically
  // imports the collector, deliberately keeping that chunk out of the
  // startup graph. The bridge awaits it like any other answer.
  __aoMemoryReport: (win) => win.__aoMemoryReport?.(),
  // How much of the reveal queue is still draining. Async for the same
  // reason as the memory report: main.ts installs it as a dynamic-import
  // stub. Unlike the diagnostics globals below it is present in every
  // build, because the measurement windows that poll it (bench, profile)
  // run against a harness binary with UI_TRACE unset.
  __aoRevealDrain: (win) => win.__aoRevealDrain?.(),
  __paneGeometry: (win) => win.__paneGeometry?.(),
  __agentOverflowTimelineMemoryStats: (win) => win.__agentOverflowTimelineMemoryStats?.(),
  __agentOverflowTimelineMemoryStatsByPane: (win) =>
    win.__agentOverflowTimelineMemoryStatsByPane?.(),
  __stickState: (win) => win.__stickState?.(),
  // Spelled as the CALL, not the object: the trace api also carries
  // enable/disable/clear/flush, and this query kind reads.
  'uiTrace.recent': (win, args) => win.__agentOverflowUiTrace?.recent?.(numArg(args, 0, 50)),
};

/** The names a `globals` query may ask for, for error messages and docs. */
export function harnessGlobalNames(): string[] {
  return Object.keys(READERS).sort();
}

export class UnknownGlobalError extends Error {
  constructor(name: string) {
    super(`unknown global ${JSON.stringify(name)} (allowed: ${harnessGlobalNames().join(', ')})`);
    this.name = 'UnknownGlobalError';
  }
}

/**
 * Reads one whitelisted global. Throws `UnknownGlobalError` for a name
 * outside the list; answers `{unavailable:true}` for a name this build
 * did not install.
 */
export async function readHarnessGlobal(
  win: HarnessWindow,
  name: string,
  args: readonly unknown[] = [],
): Promise<GlobalsResult> {
  // hasOwn, not a truthiness test on the lookup: `READERS` is an object
  // literal, so `constructor`, `toString` and `__proto__` all resolve to
  // something through Object.prototype. A plain `READERS[name]` would take
  // `constructor` for a reader and call it — which is precisely the
  // whitelist bypass this table exists to prevent.
  if (!Object.hasOwn(READERS, name)) throw new UnknownGlobalError(name);
  const reader = READERS[name]!;
  const value = await reader(win, args);
  if (value === undefined) return { v: 1, name, unavailable: true };
  return { v: 1, name, value };
}
