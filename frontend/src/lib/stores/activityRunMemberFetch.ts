// On-demand activity-run members: the RPC, its debounce, and what each
// failure means (docs/architecture/timeline-window-pages.md §3, §6).
//
// A history page ships a window of each run plus a stub for the rest.
// Three things ask the server for more: a boundary the reader clicked
// past, a jump whose target is an unshipped member, and a STUB REFRESH —
// a record the pane marked dirty because a pushed row or a crossing page
// left the stub describing a span the pane no longer holds. The first two
// mount rows; the third asks for none (`limit: 0`) and takes only the
// stub.
//
// Refreshes are debounced through one `RefreshScheduler`, so a burst of
// pushed rows into held runs costs one round trip per run rather than one
// per row, and a busy stream still lands on the scheduler's absolute
// deadline instead of starving.

import type { Item } from '../types/models';
import type { PageShape } from '../../../bindings/agent-overflow/internal/app/models';
import type { ActivityRunMembers } from '../../../bindings/agent-overflow/internal/store/models';
import {
  ListActivityRunMembers,
  type ActivityRunMembersDirection,
} from './bindings';
import {
  type ActivityRunRecord,
  type ActivityRunRecords,
} from './activityRunStubs';
import {
  createRefreshScheduler,
  type RefreshScheduler,
} from '../utils/refreshScheduler';

/**
 * The wire code `ListActivityRunMembers` refuses with when the caller's
 * picture of the run no longer matches the store's
 * (`app.ActivityRunStaleCode`). A code, not the message: a client that is
 * not on the backend's loopback is told only the public code and message,
 * and the pane branches on this refusal.
 */
export const ACTIVITY_RUN_STALE_CODE = 'activity_run_stale';

export function isStaleActivityRunError(err: unknown): boolean {
  return typeof err === 'object'
    && err !== null
    && (err as { code?: unknown }).code === ACTIVITY_RUN_STALE_CODE;
}

export interface ActivityRunMemberFetchOptions {
  /** The pane's thread, or null when it holds none. Read per call. */
  threadId(): string | null;
  /** The pane's page shape (`timelinePageShape()`). Read per call. */
  shape(): PageShape;
  /** The live record map. Read per call; the registry replaces entries in it. */
  records(): ActivityRunRecords;
  /**
   * Mount a response's rows into the pane's window. `replaces` is true for
   * an `around` answer, whose span REPLACES the loaded one: the previous
   * members become unshipped and are described by the returned stub, so
   * they must leave the window in the same commit the new ones enter it.
   */
  mountMembers(
    record: ActivityRunRecord,
    rows: readonly Item[],
    replaces: boolean,
  ): void;
  /**
   * The pane's window reload around the reader's anchor, after a stale-run
   * refusal. Rejects when the authoritative history could not be read.
   */
  reloadWindow(): Promise<void>;
  /**
   * Tell the reader a fetch failed. The boundary and the jump are reader
   * gestures, so a failure has to be visible; a background stub refresh
   * passes `silent` and leaves the record dirty for the next trigger.
   */
  reportFailure(message: string, err: unknown, silent: boolean): void;
  /**
   * A response's stub was recorded. Every count a node or a header
   * derives from the record may have moved without any row changing (a
   * `limit: 0` refresh), so the registry re-projects on it.
   */
  onStubApplied(stub: ActivityRunMembers['stub']): string[];
}

export interface FetchMembersRequest {
  direction: ActivityRunMembersDirection;
  /** Members to mount. 0 refreshes the stub only. */
  limit: number;
  /** Required for `around`: the member the replacement span centers on. */
  aroundItemId?: string;
}

export interface ActivityRunMemberFetch {
  /**
   * Fetch and mount members of one run. Resolves with the ids the pane
   * now holds for it, or an empty array when the call failed or the run
   * is no longer held — the caller renders a pending boundary until this
   * settles and reports nothing itself.
   */
  fetch(runFirstItemId: string, request: FetchMembersRequest): Promise<string[]>;
  /** Ask for a dirty record's stub to be restated. Debounced per run. */
  scheduleRefresh(): void;
  /** Thread switch: drop the cycle and every in-flight claim. */
  reset(): void;
  dispose(): void;
}

export function createActivityRunMemberFetch(
  options: ActivityRunMemberFetchOptions,
): ActivityRunMemberFetch {
  const inFlight = new Map<string, object>();
  const refreshAfterFlight = new Set<string>();
  let generation = 0;
  let disposed = false;
  let recovery: Promise<void> | null = null;
  let refreshAfterRecovery = false;

  const scheduler: RefreshScheduler = createRefreshScheduler({
    name: 'activityRunStubRefresh',
    delayMs: 200,
    maxWaitMs: 1_000,
    run: async () => {
      if (recovery) { refreshAfterRecovery = true; return; }
      await Promise.all([...options.records().values()]
        .filter(record => record.dirty)
        .map(record => call(record, { direction: 'before', limit: 0 }, true)));
    },
  });

  async function recover(err: unknown): Promise<void> {
    if (recovery) return recovery;
    // A stale refusal is a reconciliation signal. Only a failed recovery
    // needs a toast; every other refusal in this cycle joins this refresh.
    options.reportFailure('Activity changed; refreshing history', err, true);
    const gen = generation;
    const threadId = options.threadId();
    let succeeded = false;
    const pending = Promise.resolve()
      .then(async () => {
        if (disposed || generation !== gen || options.threadId() !== threadId) return;
        await options.reloadWindow();
        succeeded = true;
      })
      .catch((failure: unknown) => {
        if (!disposed && generation === gen) {
          options.reportFailure('Failed to refresh activity', failure, false);
        }
      })
      .finally(() => {
        if (recovery !== pending) return;
        recovery = null;
        const requested = refreshAfterRecovery;
        refreshAfterRecovery = false;
        if (succeeded && requested && !disposed && generation === gen) scheduler.request();
      });
    recovery = pending;
    return pending;
  }

  async function call(
    record: ActivityRunRecord,
    request: FetchMembersRequest,
    silent: boolean,
  ): Promise<string[]> {
    const threadId = options.threadId();
    if (!threadId || disposed || recovery) return [];
    const runKey = record.runFirstItemId;
    if (inFlight.has(runKey)) {
      if (silent) refreshAfterFlight.add(runKey);
      return [];
    }
    const claim = {};
    const gen = generation;
    const stub = record.stub;
    const invalidationVersion = record.invalidationVersion;
    const cutVersion = record.cutVersion;
    const current = (): boolean => !disposed && generation === gen
      && options.threadId() === threadId
      && options.records().get(runKey) === record
      && record.stub === stub
      && record.cutVersion === cutVersion;
    inFlight.set(runKey, claim);
    try {
      const answer = await ListActivityRunMembers(threadId, {
        runFirstItemId: runKey,
        loadedFirstItemId: record.loadedFirstItemId,
        loadedLastItemId: record.loadedLastItemId,
        direction: request.direction,
        aroundItemId: request.aroundItemId ?? '',
        limit: request.limit,
        shape: options.shape(),
      });
      if (!current() || recovery) return [];
      const rows = (answer.items ?? []) as Item[];
      if (rows.length > 0) options.mountMembers(record, rows, request.direction === 'around');
      // Reconcile against the actual loaded span. A live append during the
      // request must remain loaded and leave this older answer dirty.
      const invalidated = record.invalidationVersion !== invalidationVersion;
      const held = options.onStubApplied(answer.stub);
      if (invalidated && options.records().get(runKey) === record) {
        record.dirty = true;
        scheduler.request();
      }
      return held;
    } catch (err) {
      if (!current()) return [];
      if (isStaleActivityRunError(err)) await recover(err);
      else options.reportFailure('Failed to load activity', err, silent);
      return [];
    } finally {
      if (inFlight.get(runKey) === claim) {
        inFlight.delete(runKey);
        if (refreshAfterFlight.delete(runKey)) scheduler.request();
      }
    }
  }

  return {
    fetch(runFirstItemId, request) {
      const record = options.records().get(runFirstItemId);
      return record ? call(record, request, false) : Promise.resolve([]);
    },
    scheduleRefresh() {
      if (recovery) refreshAfterRecovery = true;
      else scheduler.request();
    },
    reset() {
      generation += 1;
      recovery = null;
      refreshAfterRecovery = false;
      scheduler.reset();
      inFlight.clear();
      refreshAfterFlight.clear();
    },
    dispose() {
      disposed = true;
      generation += 1;
      recovery = null;
      refreshAfterRecovery = false;
      scheduler.dispose();
      inFlight.clear();
      refreshAfterFlight.clear();
    },
  };
}
