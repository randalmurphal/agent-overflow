// The TypeScript half of the shared activity-run contract
// (docs/architecture/timeline-window-pages.md §1 and §4). The vectors are
// the same file the Go test reads
// (internal/store/testdata/activity_run_vectors.json, kept byte-identical
// by `TestActivityRunVectorsAreSharedByteForByte`), so a change to either
// implementation that does not change the other fails here.
//
// Two claims per case:
//
//  1. The client's physical run classification (`groupActivityRunSpans`)
//     finds the same runs the server does, under every rule the case
//     names (`rules`).
//  2. For every shipped span the fixture states, the header the client
//     builds from (loaded rows + stub) reports the same TOTALS as the
//     header it would build with every row loaded. A collapsed run must
//     not report one number while the rows behind it say another.
//
// The two status facts, failed and running, agree the same way: the
// stub's two launch lists cover every pair the shipped span splits.
import { describe, expect, it } from 'vitest';
import vectors from '../../test/fixtures/activityRunVectors.json';
import {
  AGENT_MESSAGES_ABSORBED_RULE,
  AGENT_MESSAGE_MEMBERS_RULE,
  groupActivityRunSpans,
  type ActivityRunRule,
} from './activityRunSpans';
import {
  activityRunSummary,
  type ActivityRunSummary,
} from '../components/chat/activityRunSummary';
import type { ActivityRunStubFacts } from '../stores/activityRunStubs';
import type { Item } from '../types/models';
import { makeItem } from '../../test/helpers/chat';

interface VectorItem {
  id: string;
  kind: string;
  toolName: string;
  status: string;
  completionOf: string;
  payloadKind: string;
  isBackground: boolean;
  meta: string;
  payloadMeta: string;
}

interface VectorStub {
  shippedIds: string[];
  memberCount: number;
  loadedFirstItemId: string;
  loadedLastItemId: string;
  unshippedBefore: number;
  unshippedAfter: number;
  unshippedGroups: { kind: string; toolName: string; mcp: string; rows: number }[];
  unshippedPairedLaunchIds: string[];
  shippedSupersededLaunchIds: string[];
  unshippedFailed: boolean;
  runningBefore: { kind: string; toolName: string; mcp: string } | null;
  runningAfter: { kind: string; toolName: string; mcp: string } | null;
}

interface VectorRun {
  firstItemId: string;
  lastItemId: string;
  memberIds: string[];
  stubs: VectorStub[];
}

interface VectorCase {
  name: string;
  why: string;
  rules: ActivityRunRule[];
  items: VectorItem[];
  runs: VectorRun[];
}

const cases = vectors.cases as unknown as VectorCase[];
const RULES: readonly ActivityRunRule[] = [AGENT_MESSAGE_MEMBERS_RULE, AGENT_MESSAGES_ABSORBED_RULE];

/** One fixture row as the pane holds it: a visible top-level item. */
function itemOf(row: VectorItem, index: number): Item {
  return makeItem({
    id: row.id,
    threadId: 't',
    turnIndex: 0,
    itemIndex: index,
    kind: row.kind,
    role: row.kind === 'user_text' ? 'user' : 'assistant',
    status: row.status as Item['status'],
    summary: row.id,
    toolName: row.toolName,
    completionOf: row.completionOf,
    isBackground: row.isBackground,
    meta: row.meta,
    payloadKind: row.payloadKind,
    payloadMeta: row.payloadMeta,
    createdAt: index + 1,
  });
}

/** The stub's fields as the pane reads them. No shed rows: a page ships or counts. */
function factsOf(stub: VectorStub): ActivityRunStubFacts {
  return {
    memberCount: stub.memberCount,
    unshippedBefore: stub.unshippedBefore,
    unshippedAfter: stub.unshippedAfter,
    unshippedGroups: stub.unshippedGroups,
    unshippedPairedLaunchIds: stub.unshippedPairedLaunchIds,
    shippedSupersededLaunchIds: stub.shippedSupersededLaunchIds,
    unshippedFailed: stub.unshippedFailed,
    runningBefore: stub.runningBefore,
    runningAfter: stub.runningAfter,
    shed: [],
    loadedFirstItemId: stub.loadedFirstItemId,
    loadedLastItemId: stub.loadedLastItemId,
  };
}

/** Counts as a comparable object, independent of entry ordering. */
function countsOf(summary: ActivityRunSummary): { total: number; byLabel: Record<string, number> } {
  const byLabel: Record<string, number> = {};
  for (const entry of summary.counts.entries) byLabel[entry.key] = entry.count;
  return { total: summary.counts.total, byLabel };
}

describe('activity-run shared vectors', () => {
  it('carries cases for every rule, and names no other', () => {
    for (const rule of RULES) expect(cases.some(vector => vector.rules.includes(rule))).toBe(true);
    for (const vector of cases) {
      expect(vector.rules.length).toBeGreaterThan(0);
      for (const rule of vector.rules) expect(RULES).toContain(rule);
    }
  });

  for (const vector of cases) {
    describe(vector.name, () => {
      const items = vector.items.map(itemOf);
      const itemById = new Map(items.map((item) => [item.id, item]));

      it.each(vector.rules)('classifies the same runs the server does under %s', (rule) => {
        const spans = groupActivityRunSpans(items, rule);
        expect(
          spans.map((span) => ({
            firstItemId: span.firstItemId,
            lastItemId: span.lastItemId,
            memberIds: span.items.map((item) => item.id),
          })),
        ).toEqual(
          vector.runs.map((run) => ({
            firstItemId: run.firstItemId,
            lastItemId: run.lastItemId,
            memberIds: run.memberIds,
          })),
        );
      });

      for (const run of vector.runs) {
        const runItems = run.memberIds.map((id) => itemById.get(id)!);
        const whole = activityRunSummary(runItems, null);

        for (const stub of run.stubs) {
          const shipped = stub.shippedIds.join(',') || '(none)';
          const loaded = stub.shippedIds.map((id) => itemById.get(id)!);
          const partial = activityRunSummary(loaded, null, factsOf(stub));

          it(`run ${run.firstItemId} shipping [${shipped}] counts what the whole run counts`, () => {
            expect(countsOf(partial)).toEqual(countsOf(whole));
          });

          it(`run ${run.firstItemId} shipping [${shipped}] reports the same status facts`, () => {
            expect({ hasFailure: partial.hasFailure, runningLabel: partial.runningLabel })
              .toEqual({ hasFailure: whole.hasFailure, runningLabel: whole.runningLabel });
          });
        }
      }
    });
  }
});
