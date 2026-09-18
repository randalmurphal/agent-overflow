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
  applyMembersStub,
  type ActivityRunRecord,
  type ActivityRunRecords,
} from './activityRunStubs';
import {
  createRefreshScheduler,
  type RefreshScheduler,
} from '../utils/refreshScheduler';
import { errString } from '../utils/errors';

/**
 * The sentence `internal/app.errActivityRunChanged` ends with. The run no
 * longer starts where the caller thinks, or the span it named is not
 * membership any more: every count the pane would fold into a header and
 * into its held window is then a claim about rows that moved, so the
 * answer is a window reload rather than a retry.
 */
const STALE_RUN_MESSAGE = 'this activity run changed while it was loading';

export function isStaleActivityRunError(err: unknown): boolean {
  return errString(err).includes(STALE_RUN_MESSAGE);
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
   * Returns the ids the window now holds for this run.
   */
  mountMembers(
    record: ActivityRunRecord,
    rows: readonly Item[],
    replaces: boolean,
  ): string[];
  /**
   * The pane's window reload around the reader's anchor, after a stale-run
   * refusal. Reports its own failure through the pane's toast path.
   */
  reloadWindow(): void;
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
  onStubApplied(): void;
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
  // One claim per run. A second ask for a run already in flight is
  // refused rather than queued: the answer in flight describes a span
  // that is about to change, and two answers for one run can land out of
  // order. A record still dirty when its refresh settles is picked up by
  // the trailing cycle the scheduler's dirty bit guarantees.
  const inFlight = new Set<string>();

  const scheduler: RefreshScheduler = createRefreshScheduler({
    name: 'activityRunStubRefresh',
    delayMs: 200,
    maxWaitMs: 1_000,
    run: async () => {
      const records = options.records();
      const pending: Promise<void>[] = [];
      for (const record of records.values()) {
        if (!record.dirty || inFlight.has(record.runFirstItemId)) continue;
        pending.push(refreshOne(record));
      }
      if (pending.length === 0) return;
      await Promise.all(pending);
      // A record marked dirty again while its refresh was in flight — or
      // skipped above because it was — is answered by the next cycle.
      for (const record of options.records().values()) {
        if (record.dirty) {
          scheduler.request();
          return;
        }
      }
    },
  });

  async function refreshOne(record: ActivityRunRecord): Promise<void> {
    await call(record, { direction: 'before', limit: 0 }, true);
  }

  /**
   * One `ListActivityRunMembers` round trip for `record`.
   *
   * The record is re-read from the map after the await: a thread switch or
   * a window cut can have dropped it, and applying a stub to a record
   * nobody holds would resurrect a run the pane no longer shows.
   */
  async function call(
    record: ActivityRunRecord,
    request: FetchMembersRequest,
    silent: boolean,
  ): Promise<string[]> {
    const threadId = options.threadId();
    if (!threadId) return [];
    const runKey = record.runFirstItemId;
    if (inFlight.has(runKey)) return [];
    inFlight.add(runKey);
    let answer: ActivityRunMembers;
    try {
      answer = await ListActivityRunMembers(threadId, {
        runFirstItemId: runKey,
        loadedFirstItemId: record.loadedFirstItemId,
        loadedLastItemId: record.loadedLastItemId,
        direction: request.direction,
        aroundItemId: request.aroundItemId ?? '',
        limit: request.limit,
        shape: options.shape(),
      });
    } catch (err) {
      // A stale run is not a transport fault and not retryable: the pane's
      // picture of the run is wrong, so the window is reloaded around the
      // reader. Every other failure leaves the record dirty, which is what
      // makes the next trigger retry it.
      if (isStaleActivityRunError(err)) {
        options.reportFailure('Activity moved while it was loading', err, false);
        options.reloadWindow();
      } else {
        options.reportFailure('Failed to load activity', err, silent);
      }
      return [];
    } finally {
      inFlight.delete(runKey);
    }
    if (options.threadId() !== threadId) return [];
    const current = options.records().get(runKey);
    if (!current) return [];
    const rows = (answer.items ?? []) as Item[];
    const mounted = rows.length > 0
      ? options.mountMembers(current, rows, request.direction === 'around')
      : [];
    // Last: the stub describes the span the pane holds AFTER the rows
    // mounted, so recording it before the mount would describe a window
    // that does not exist yet.
    applyMembersStub(options.records(), answer.stub);
    options.onStubApplied();
    return mounted;
  }

  return {
    fetch(runFirstItemId, request) {
      const record = options.records().get(runFirstItemId);
      if (!record) return Promise.resolve([]);
      return call(record, request, false);
    },
    scheduleRefresh() {
      scheduler.request();
    },
    reset() {
      scheduler.reset();
      inFlight.clear();
    },
    dispose() {
      scheduler.dispose();
      inFlight.clear();
    },
  };
}
