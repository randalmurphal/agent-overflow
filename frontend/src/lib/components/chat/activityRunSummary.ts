// What an activity run's header says about itself: per-tool counts, plus
// the two facts a collapsed run must never hide — that something failed, and
// something is still going.
//
// Deliberately NOT part of the run node. Every value here moves on ordinary
// streaming deltas (a call completes, a status flips, a tool starts), so
// baking them into the projected node would rebuild the virtualizer's data
// array on every chunk. The header resolves current items and calls this
// instead, the same way leaf rows resolve their own items.

import type { Item } from '../../types/models';
import type { ProviderID } from '../../types/providers';
import { fileChangeDisplayRowCount } from '../../utils/fileChangeRows';
import { classifyToolName, type ToolKindIcon } from './toolCardHeader';

const THINKING_LABEL = 'thinking';
const UNNAMED_TOOL_LABEL = 'Tool';
const WAIT_LABEL = 'Wait';

/** One `14 Bash` term of a run's header line. */
export interface ActivityRunCountEntry {
  /** Stable presentation identity, including reasoning-vs-tool kind. */
  key: string;
  label: string;
  count: number;
  icon: ToolKindIcon;
  /** These rows are thinking, not a tool call. */
  isThinking: boolean;
}

/** Presented tool identity → row count, for a run's header line. */
export interface ActivityRunCounts {
  /** Count-descending, thinking last. */
  entries: ActivityRunCountEntry[];
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

interface ActivityRunPresentation {
  key: string;
  label: string;
  icon: ToolKindIcon;
  isThinking: boolean;
}

const THINKING_PRESENTATION: ActivityRunPresentation = {
  key: 'thinking',
  label: THINKING_LABEL,
  icon: 'brain',
  isThinking: true,
};

const WAIT_PRESENTATION: ActivityRunPresentation = {
  key: 'tool:clock:wait',
  label: WAIT_LABEL,
  icon: 'clock',
  isThinking: false,
};

function capitalizedHeaderLabel(label: string): string {
  if (label === 'mcp') return 'MCP';
  return label.length > 0
    ? `${label[0].toUpperCase()}${label.slice(1)}`
    : UNNAMED_TOOL_LABEL;
}

/**
 * Presentation identity for one activity item.
 *
 * Claude's wire names are already the useful header vocabulary (`Bash`,
 * `Read`, `ScheduleWakeup`), so keep them rather than collapsing unknown
 * native tools into the generic row label. Codex item names are protocol
 * categories (`command_execution`, `file_change`, `collab_agent`), so its
 * header uses the same classifier aliases as the rows. Terminal interactions
 * carry no tool name at all and need their item-kind label explicitly.
 */
function activityRunPresentation(
  item: Item,
  provider: ProviderID | null | undefined,
  cache: Map<string, ActivityRunPresentation>,
): ActivityRunPresentation {
  if (item.kind === 'thinking') return THINKING_PRESENTATION;

  if (item.kind === 'terminal_interaction') return WAIT_PRESENTATION;

  const rawName = item.toolName?.trim() ?? '';
  const sourceKey = rawName || UNNAMED_TOOL_LABEL;
  const cached = cache.get(sourceKey);
  if (cached) return cached;

  let label: string;
  let icon: ToolKindIcon;
  switch (provider) {
    case 'codex': {
      const visual = classifyToolName(rawName);
      label = capitalizedHeaderLabel(visual.label);
      icon = visual.icon;
      break;
    }
    case 'claude':
    case 'claude-tui':
    case null:
    case undefined:
      label = capitalizedHeaderLabel(rawName || UNNAMED_TOOL_LABEL);
      icon = classifyToolName(rawName).icon;
      break;
    default: {
      const exhaustive: never = provider;
      return exhaustive;
    }
  }

  const presentation: ActivityRunPresentation = {
    key: `tool:${icon}:${label}`,
    label,
    icon,
    isThinking: false,
  };
  cache.set(sourceKey, presentation);
  return presentation;
}

function isFailedStatus(status: Item['status']): boolean {
  // `declined` is a user decision, not a failure; `killed` and `errored`
  // are outcomes the user did not choose and a collapsed run must not hide.
  return status === 'errored' || status === 'killed';
}

/**
 * `ActivityRunSummary.hasFailure` alone, without building the counts — for
 * the auto-collapse gate, which asks per run per pass and needs nothing else.
 * Same failure rule as the summary; an item that has left the window cannot
 * fail a run it is no longer in.
 */
export function activityRunHasFailure(
  itemIds: readonly string[],
  getItem: (id: string) => Item | undefined,
): boolean {
  for (const id of itemIds) {
    const item = getItem(id);
    if (item && isFailedStatus(item.status)) return true;
  }
  return false;
}

function isRunningStatus(status: Item['status']): boolean {
  return status === 'running' || status === 'streaming';
}

/**
 * Per-presentation aggregation for the header line, e.g.
 * `14 Bash, 6 Read, 9 thinking`.
 *
 * A `tool_completion` pairs with its call and is not counted separately —
 * one Bash call that finished is one Bash, not two. A completion whose call
 * is outside the run is an orphan and counts under its own presented tool
 * identity, so a run trimmed at the head still reports honestly.
 */
export function activityRunSummary(
  items: readonly Item[],
  provider: ProviderID | null | undefined,
): ActivityRunSummary {
  const presentIds = new Set(items.map((item) => item.id));
  // A run can hold hundreds of repeated Bash/Edit rows and this summary
  // re-evaluates on streaming deltas. Classify each distinct source name once
  // per pass; a module-level cache would be unbounded by provider input.
  const presentationCache = new Map<string, ActivityRunPresentation>();
  const buckets = new Map<string, ActivityRunCountEntry>();
  let total = 0;
  let hasFailure = false;
  let runningLabel: string | null = null;

  for (const item of items) {
    if (isFailedStatus(item.status)) hasFailure = true;
    // Last one wins: the newest active row is what the user wants named.
    if (isRunningStatus(item.status)) {
      runningLabel = activityRunPresentation(item, provider, presentationCache).label;
    }

    if (item.kind === 'tool_completion') {
      const callId = item.completionOf;
      if (callId && presentIds.has(callId)) continue;
    }
    const displayRowCount = fileChangeDisplayRowCount(item);
    total += displayRowCount;
    const presentation = activityRunPresentation(item, provider, presentationCache);
    const bucket = buckets.get(presentation.key);
    if (bucket) bucket.count += displayRowCount;
    else buckets.set(presentation.key, { ...presentation, count: displayRowCount });
  }

  const entries = [...buckets.values()]
    .sort((a, b) => {
      // Thinking last regardless of count: it is ambient, and a reader
      // scanning a header wants the tools first.
      const aThinking = a.isThinking;
      const bThinking = b.isThinking;
      if (aThinking !== bThinking) return aThinking ? 1 : -1;
      if (a.count !== b.count) return b.count - a.count;
      return a.label.localeCompare(b.label);
    });

  return { counts: { entries, total }, hasFailure, runningLabel };
}
