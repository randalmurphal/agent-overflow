import type { Item } from '../types/models';
import type { ActivityRunStub } from '../../../bindings/agent-overflow/internal/store/models';
import type { ActivityRunRecords } from './activityRunStubs';
import { compareCursors, type TimelineCursorLike } from './threadItems';

interface RunRange {
  stub: ActivityRunStub;
  first: TimelineCursorLike;
  last: TimelineCursorLike;
  priority: number;
  latestEnd: TimelineCursorLike;
}

/** An ephemeral index for one projection. Overlapping stale descriptions
 * retain their original precedence until reconciliation drops the loser. */
export function createActivityRunLookup(records: ActivityRunRecords, stubs: readonly ActivityRunStub[]) {
  const descriptions = new Map<string, ActivityRunStub>();
  for (const record of records.values()) descriptions.set(record.runFirstItemId, record.stub);
  for (const stub of stubs) descriptions.set(stub.firstItemId, stub);
  const ranges: RunRange[] = [];
  for (const stub of descriptions.values()) {
    const last = { turnIndex: stub.lastTurnIndex, itemIndex: stub.lastItemIndex };
    ranges.push({ stub, priority: ranges.length, last, latestEnd: last,
      first: { turnIndex: stub.firstTurnIndex, itemIndex: stub.firstItemIndex } });
  }
  ranges.sort((a, b) => compareCursors(a.first, b.first));
  for (let i = 1; i < ranges.length; i += 1) {
    if (compareCursors(ranges[i - 1].latestEnd, ranges[i].last) > 0) {
      ranges[i].latestEnd = ranges[i - 1].latestEnd;
    }
  }
  return {
    get size() { return ranges.length; },
    find(item: Pick<Item, 'turnIndex' | 'itemIndex'>): ActivityRunStub | undefined {
      let lo = 0;
      let hi = ranges.length;
      while (lo < hi) {
        const mid = (lo + hi) >>> 1;
        if (compareCursors(ranges[mid].first, item) <= 0) lo = mid + 1;
        else hi = mid;
      }
      let match: RunRange | undefined;
      for (let i = lo - 1; i >= 0 && compareCursors(ranges[i].latestEnd, item) >= 0; i -= 1) {
        const range = ranges[i];
        if (compareCursors(range.last, item) >= 0 && (!match || range.priority < match.priority)) match = range;
      }
      return match?.stub;
    },
  };
}

export type ActivityRunLookup = ReturnType<typeof createActivityRunLookup>;
