// A backend that classifies a thread's history into activity runs itself,
// for tests that drive a real pane against a backend whose membership rule
// may differ from the client's. It answers ListThreadSliceAround with the
// whole history and a stub per run, and ListActivityRunMembers the way the
// store does: refused as stale when the named run or loaded span is not
// the server's (docs/architecture/timeline-window-pages.md §3).
//
// The classifier is written out here rather than imported from the
// client's predicates, so a test can pit the pane against a rule the
// client does not apply.

import { vi } from 'vitest';
import { setBindingMock } from '../mocks/bindings-app';
import { windowDigest } from '../../lib/stores/threadWindowDigest';
import { isAgentMessageItem } from '../../lib/utils/agentMessage';
import type { Item } from '../../lib/types/models';
import type { ActivityRunStub } from '../../../bindings/agent-overflow/internal/store/models';

const RAIL_KINDS = new Set(['tool_call', 'tool_completion', 'terminal_interaction', 'thinking']);

/**
 * Whether this backend classifies by the member-on-sight rule for a
 * request: a fixed answer, or one read off the request (its page shape or
 * members request).
 */
export type AgentMessageRuleOf = (request: Record<string, unknown>) => boolean;

function railRow(row: Item): boolean {
  return RAIL_KINDS.has(row.kind) && row.payloadKind !== 'proposed_plan';
}

/** Maximal runs of `rows` under one rule. */
export function classifyRuns(rows: readonly Item[], agentMessageMembers: boolean): Item[][] {
  const runs: Item[][] = [];
  let open: Item[] | null = null;
  for (const row of rows) {
    const agent = isAgentMessageItem(row);
    const member = railRow(row) || (agentMessageMembers && agent);
    const absorbed = row.kind === 'notification' || (!agentMessageMembers && agent);
    if (member || (open !== null && absorbed)) {
      if (open === null) {
        open = [];
        runs.push(open);
      }
      open.push(row);
      continue;
    }
    open = null;
  }
  return runs;
}

function stubFor(run: readonly Item[], loadedFirst: number, loadedLast: number): ActivityRunStub {
  const unshipped = [...run.slice(0, loadedFirst), ...run.slice(loadedLast + 1)];
  const first = run[0];
  const last = run[run.length - 1];
  return {
    firstItemId: first.id,
    lastItemId: last.id,
    firstTurnIndex: first.turnIndex,
    firstItemIndex: first.itemIndex,
    lastTurnIndex: last.turnIndex,
    lastItemIndex: last.itemIndex,
    memberCount: run.length,
    loadedFirstItemId: run[loadedFirst]?.id ?? '',
    loadedLastItemId: run[loadedLast]?.id ?? '',
    unshippedBefore: loadedFirst,
    unshippedAfter: run.length - 1 - loadedLast,
    unshippedDigest: windowDigest(unshipped.map(row => ({ id: row.id, rev: row.rev ?? 0 }))),
    unshippedGroups: [],
    unshippedPairedLaunchIds: [],
    shippedSupersededLaunchIds: [],
    unshippedFailed: false,
    runningBefore: null,
    runningAfter: null,
  } as ActivityRunStub;
}

function staleError(message: string): Error {
  return Object.assign(new Error(message), { code: 'activity_run_stale' });
}

export interface ClassifyingBackend {
  /** Every window page the pane asked for. */
  readonly pages: ReturnType<typeof vi.fn>;
  /** Every members call the pane made. */
  readonly members: ReturnType<typeof vi.fn>;
}

/**
 * Serve `history()` (the thread's whole top-level history, in timeline
 * order) through the pane's page and members calls, classified by
 * `agentMessageMembers`.
 */
export function serveClassifiedHistory(
  history: () => readonly Item[],
  agentMessageMembers: boolean | AgentMessageRuleOf,
): ClassifyingBackend {
  const ruleOf: AgentMessageRuleOf = typeof agentMessageMembers === 'function'
    ? agentMessageMembers
    : () => agentMessageMembers;
  const page = (options: Record<string, unknown>) => {
    const rows = [...history()];
    const runs = classifyRuns(rows, ruleOf(options)).map(run => stubFor(run, 0, run.length - 1));
    const oldest = rows[0];
    const newest = rows[rows.length - 1];
    return {
      items: rows,
      runs,
      oldestTurnIndex: oldest?.turnIndex ?? -1,
      newestTurnIndex: newest?.turnIndex ?? -1,
      hasMore: false,
      hasMoreOlder: false,
      hasMoreNewer: false,
      oldestCursor: { turnIndex: oldest?.turnIndex ?? 0, itemIndex: oldest?.itemIndex ?? 0, itemId: oldest?.id ?? '' },
      newestCursor: { turnIndex: newest?.turnIndex ?? 0, itemIndex: newest?.itemIndex ?? 0, itemId: newest?.id ?? '' },
    };
  };
  const pages = vi.fn(async (_threadId: string, _anchor: string, _budget: number, options: Record<string, unknown>) =>
    page(options ?? {}));
  const members = vi.fn(async (_threadId: string, req: Record<string, unknown>) => {
    const runs = classifyRuns([...history()], ruleOf(req));
    const run = runs.find(candidate => candidate[0].id === req.runFirstItemId);
    if (!run) throw staleError(`activity run ${String(req.runFirstItemId)} no longer starts there`);
    const ids = run.map(row => row.id);
    const first = req.loadedFirstItemId ? ids.indexOf(String(req.loadedFirstItemId)) : -1;
    const last = req.loadedLastItemId ? ids.indexOf(String(req.loadedLastItemId)) : -1;
    if ((req.loadedFirstItemId && first < 0) || (req.loadedLastItemId && last < 0)) {
      throw staleError(`loaded span is not a span of activity run ${String(req.runFirstItemId)}`);
    }
    if (Number(req.limit ?? 0) !== 0) throw new Error('classifying backend answers stub refreshes only');
    if (first < 0) return { items: [], stub: { ...stubFor(run, run.length, run.length - 1), loadedLastItemId: '' } };
    return { items: [], stub: stubFor(run, first, last) };
  });
  setBindingMock('ListThreadSliceAround', pages);
  setBindingMock('ListActivityRunMembers', members);
  return { pages, members };
}
