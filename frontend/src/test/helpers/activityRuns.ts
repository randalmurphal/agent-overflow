// Shared fixtures for the activity-run registry tests. Extracted so the
// identity/collapse suite and the summary-signal suite drive the registry
// through ONE description of a projection pass — two copies would drift,
// and the pass order (identity and window per run, then collapse once
// the tail is known) is exactly what these tests are pinning.

import type { ActivityRunResolution } from '../../lib/utils/activityRunGrouping';
import { createThreadActivityRuns } from '../../lib/stores/threadActivityRuns.svelte';
import type { PaneScrollController } from '../../lib/stores/threadPaneShared';

export type ActivityRunRegistry = ReturnType<typeof createThreadActivityRuns>;

export function registry(
  overrides: {
    defaultCollapsed?: boolean;
    windowRows?: number;
    /**
     * Default null: `withViewportBottomHeld` falls back to running the change
     * bare, so registry logic tests need no controller. Pass one to assert
     * the mutators' viewport hold itself.
     */
    scrollController?: PaneScrollController;
  } = {},
): ActivityRunRegistry {
  return createThreadActivityRuns({
    defaultCollapsed: () => overrides.defaultCollapsed ?? false,
    windowRows: () => overrides.windowRows ?? 30,
    scrollController: () => overrides.scrollController ?? null,
  });
}

/**
 * One run, described row by row. A bare id is a one-item row; an array is a
 * group row carrying several items, which is the case that makes a run's row
 * count differ from its member count.
 */
export type RunSpec = (string | string[])[];

/**
 * One projection pass, resolved in the order `groupActivityRuns` resolves it:
 * identity and window per run, then collapse once the tail is known.
 *
 * `tailIndex` names the run at the window's revealed tail — at most one can
 * be, and `-1` means the pass has none (a window whose last row is prose).
 */
export function pass(
  runs: ActivityRunRegistry,
  specs: RunSpec[],
  threadId = 'thread-1',
  tailIndex = -1,
): (ActivityRunResolution & { collapsed: boolean })[] {
  runs.beginPass();
  const resolved = specs.map((spec) =>
    runs.resolve(spec.map((row) => (typeof row === 'string' ? [row] : row)), threadId),
  );
  const out = resolved.map((run, index) => ({
    ...run,
    collapsed: runs.collapsedFor(run.runId, index === tailIndex),
  }));
  runs.endPass();
  return out;
}

/** A run of `n` single-item rows, with ids stable across passes. */
export function rows(n: number): RunSpec {
  return Array.from({ length: n }, (_, i) => `i${i}`);
}
