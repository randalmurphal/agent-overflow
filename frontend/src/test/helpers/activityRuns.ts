import { makeItem } from './chat';
import { windowDigest } from '../../lib/stores/threadWindowDigest';
import type { ActivityRunStub } from '../../../bindings/agent-overflow/internal/store/models';
// Shared fixtures for activity-run projection, membership, and recovery tests.

import type { ActivityRunResolution } from '../../lib/utils/activityRunGrouping';
import { createThreadActivityRuns } from '../../lib/stores/threadActivityRuns.svelte';
import type { ThreadActivityRunsOptions } from '../../lib/stores/threadActivityRuns.svelte';
import type { PaneScrollController } from '../../lib/stores/threadPaneShared';
import type { Item } from '../../lib/types/models';

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
    /**
     * Default true: the window is verified, so a tail run records its
     * open hold. Pass a getter to drive a switch's cached-paint phase.
     */
    windowVerified?: () => boolean;
    /**
     * The pane's loaded window. Only the run-record half of the registry
     * reads it; the identity/collapse suites drive `resolve` directly and
     * leave it empty.
     */
    items?: () => readonly Item[];
    /** Default null, which makes every members fetch a no-op. */
    threadId?: () => string | null;
    /** Records every mount a members answer asked for. */
    mountRunMembers?: ThreadActivityRunsOptions['mountRunMembers'];
    reloadWindow?: ThreadActivityRunsOptions['reloadWindow'];
    reportFetchFailure?: ThreadActivityRunsOptions['reportFetchFailure'];
  } = {},
): ActivityRunRegistry {
  return createThreadActivityRuns({
    defaultCollapsed: () => overrides.defaultCollapsed ?? false,
    windowRows: () => overrides.windowRows ?? 30,
    windowVerified: overrides.windowVerified ?? (() => true),
    scrollController: () => overrides.scrollController ?? null,
    items: overrides.items ?? (() => []),
    threadId: overrides.threadId ?? (() => null),
    pageShape: () => ({ inlinePreviews: true, runWindowRows: 5, maxBytes: 1024 }),
    mountRunMembers: overrides.mountRunMembers ?? (() => {}),
    reloadWindow: overrides.reloadWindow ?? (async () => {}),
    windowBounds: () => ({ oldest: null, newest: null }),
    reportFetchFailure: overrides.reportFetchFailure ?? (() => {}),
  });
}

/**
 * The five run-record fields `resolve` stamps on a node, as a run the
 * pane holds WHOLE reports them. Node literals in tests spread this so
 * adding a field to the contract does not rewrite every fixture.
 */
export function wholeRunNodeFields(memberIds: readonly string[] = []) {
  return {
    memberCount: memberIds.length,
    unshippedBefore: 0,
    unshippedAfter: 0,
    loadedFirstItemId: memberIds[0] ?? '',
    loadedLastItemId: memberIds[memberIds.length - 1] ?? '',
  };
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

export function activityRunRow(id: string, index: number, overrides: Partial<Item> = {}): Item {
  return makeItem({
    id,
    threadId: 't',
    turnIndex: 0,
    itemIndex: index,
    kind: 'tool_call',
    toolName: 'Bash',
    rev: index + 1,
    ...overrides,
  });
}

export function activityRunProse(id: string, index: number): Item {
  return activityRunRow(id, index, { kind: 'assistant_text', toolName: '' });
}

export function activityRunStub(overrides: Partial<ActivityRunStub> = {}): ActivityRunStub {
  return {
    firstItemId: 'a',
    lastItemId: 'e',
    firstTurnIndex: 0,
    firstItemIndex: 1,
    lastTurnIndex: 0,
    lastItemIndex: 5,
    memberCount: 5,
    loadedFirstItemId: 'b',
    loadedLastItemId: 'd',
    unshippedBefore: 1,
    unshippedAfter: 1,
    unshippedDigest: windowDigest([{ id: 'a', rev: 1 }, { id: 'e', rev: 5 }]),
    unshippedGroups: [],
    unshippedPairedLaunchIds: [],
    shippedSupersededLaunchIds: [],
    unshippedFailed: false,
    runningBefore: null,
    runningAfter: null,
    ...overrides,
  } as ActivityRunStub;
}
