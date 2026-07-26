// What a collapsed activity run says about itself: per-tool counts, plus
// the two facts a chip must never hide — that something failed, and that
// something is still going.
//
// Deliberately NOT part of the run node. Every value here moves on ordinary
// streaming deltas (a call completes, a status flips, a tool starts), so
// baking them into the projected node would rebuild the virtualizer's data
// array on every chunk. The chip resolves current items and calls this
// instead, the same way leaf rows resolve their own items.

import type { Item } from '../types/models';

const THINKING_LABEL = 'thinking';
const UNNAMED_TOOL_LABEL = 'tool';

/** Tool display name → row count, for the collapsed chip line. */
export interface ActivityRunCounts {
  /** Count-descending, thinking last. */
  entries: { label: string; count: number }[];
  /** Total rows represented, including every group member. */
  total: number;
}

export interface ActivityRunSummary {
  counts: ActivityRunCounts;
  /** A member tool call failed. */
  hasFailure: boolean;
  /** Tool name of the newest still-running member, or null. */
  runningLabel: string | null;
}

export function activityRunToolLabel(item: Item): string {
  if (item.kind === 'thinking') return THINKING_LABEL;
  const name = item.toolName?.trim();
  return name && name.length > 0 ? name : UNNAMED_TOOL_LABEL;
}

function isFailedStatus(status: Item['status']): boolean {
  // `declined` is a user decision, not a failure; `killed` and `errored`
  // are outcomes the user did not choose and must not be hidden by a chip.
  return status === 'errored' || status === 'killed';
}

function isRunningStatus(status: Item['status']): boolean {
  return status === 'running' || status === 'streaming';
}

/**
 * Per-tool aggregation for the chip line, e.g. `14 Bash, 6 Read, 9 thinking`.
 *
 * A `tool_completion` pairs with its call and is not counted separately —
 * one Bash call that finished is one Bash, not two. A completion whose call
 * is outside the run is an orphan and counts under its own tool name, so a
 * run trimmed at the head still reports honestly.
 */
export function activityRunSummary(items: readonly Item[]): ActivityRunSummary {
  const presentIds = new Set(items.map((item) => item.id));
  const byLabel = new Map<string, number>();
  let total = 0;
  let hasFailure = false;
  let runningLabel: string | null = null;

  for (const item of items) {
    if (isFailedStatus(item.status)) hasFailure = true;
    // Last one wins: the newest active row is what the user wants named.
    if (isRunningStatus(item.status)) runningLabel = activityRunToolLabel(item);

    if (item.kind === 'tool_completion') {
      const callId = item.completionOf;
      if (callId && presentIds.has(callId)) continue;
    }
    total += 1;
    const label = activityRunToolLabel(item);
    byLabel.set(label, (byLabel.get(label) ?? 0) + 1);
  }

  const entries = [...byLabel.entries()]
    .map(([label, count]) => ({ label, count }))
    .sort((a, b) => {
      // Thinking last regardless of count: it is ambient, and a reader
      // scanning a chip wants the tools first.
      const aThinking = a.label === THINKING_LABEL;
      const bThinking = b.label === THINKING_LABEL;
      if (aThinking !== bThinking) return aThinking ? 1 : -1;
      if (a.count !== b.count) return b.count - a.count;
      return a.label.localeCompare(b.label);
    });

  return { counts: { entries, total }, hasFailure, runningLabel };
}
