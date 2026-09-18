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
import { aoToolPresentation } from './aoTools';
import { parseJsonObject } from '../../utils/parseJsonObject';
import type { ActivityRunStubFacts, ShedRow } from '../../stores/activityRunStubs';

/** Shared empty list: most runs are held whole and shed nothing. */
const EMPTY_SHED: readonly ShedRow[] = Object.freeze([]);

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

// Notification bells absorbed into a run (`activityRunGrouping.ts`
// `isAbsorbedNotification`): a Monitor ping or background-task bell that
// landed mid-run. Label pluralized in the entry post-pass — "2 notification"
// reads as a typo where "12 thinking" does not.
const NOTIFICATION_PRESENTATION: ActivityRunPresentation = {
  key: 'notification',
  label: 'notification',
  icon: 'speech-bubble',
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
 *
 * A tool one of AO's own MCP servers served presents by family and verb
 * (`Remote run`, `Browser click`) on both providers: the wire name
 * `MCP/remote_run` is an encoding, not vocabulary.
 */
function activityRunPresentation(
  item: Item,
  provider: ProviderID | null | undefined,
  cache: Map<string, ActivityRunPresentation>,
): ActivityRunPresentation {
  const rawName = item.toolName?.trim() ?? '';
  return presentationFor(
    item.kind,
    rawName,
    // Only MCP rows carry `meta.mcp`; parsing every native tool's meta on
    // each streaming delta would be waste.
    rawName.startsWith('MCP') ? parseJsonObject(item.meta) : null,
    provider,
    cache,
  );
}

/**
 * The same presentation from a raw `(kind, toolName, mcp)` identity: the
 * server's `ActivityRunGroupKey`, and the narrow copy a shed row keeps.
 *
 * One reading of the rule for both halves of a half-loaded run's header.
 * `mcp` is the `{server, tool}` object as JSON text — exactly
 * `json_extract(items.meta, '$.mcp')`, which is the field the server puts
 * on the key — and "" for a native tool.
 */
export function presentationForGroupKey(
  key: { kind: string; toolName: string; mcp: string },
  provider: ProviderID | null | undefined,
  cache: Map<string, ActivityRunPresentation>,
): ActivityRunPresentation {
  const rawName = key.toolName.trim();
  const mcp = key.mcp === '' ? null : parseJsonObject(key.mcp);
  return presentationFor(
    key.kind,
    rawName,
    mcp === null ? null : { mcp },
    provider,
    cache,
  );
}

function presentationFor(
  kind: string,
  rawName: string,
  mcpMeta: Record<string, unknown> | null,
  provider: ProviderID | null | undefined,
  cache: Map<string, ActivityRunPresentation>,
): ActivityRunPresentation {
  if (kind === 'thinking') return THINKING_PRESENTATION;
  if (kind === 'terminal_interaction') return WAIT_PRESENTATION;
  if (kind === 'notification') return NOTIFICATION_PRESENTATION;

  const ao = mcpMeta === null ? null : aoToolPresentation(mcpMeta);
  const sourceKey = ao ? `${rawName}@${ao.server}` : rawName || UNNAMED_TOOL_LABEL;
  const cached = cache.get(sourceKey);
  if (cached) return cached;

  const { label, icon } = ao
    ? { label: ao.headerLabel, icon: ao.icon }
    : nativeToolPresentation(rawName, provider);
  const presentation: ActivityRunPresentation = {
    key: `tool:${icon}:${label}`,
    label,
    icon,
    isThinking: false,
  };
  cache.set(sourceKey, presentation);
  return presentation;
}

function nativeToolPresentation(
  rawName: string,
  provider: ProviderID | null | undefined,
): { label: string; icon: ToolKindIcon } {
  switch (provider) {
    case 'codex': {
      const visual = classifyToolName(rawName);
      return { label: capitalizedHeaderLabel(visual.label), icon: visual.icon };
    }
    case 'claude':
    case 'claude-tui':
    case null:
    case undefined:
      return {
        label: capitalizedHeaderLabel(rawName || UNNAMED_TOOL_LABEL),
        icon: classifyToolName(rawName).icon,
      };
    default: {
      const exhaustive: never = provider;
      return exhaustive;
    }
  }
}

function addRows(
  buckets: Map<string, ActivityRunCountEntry>,
  presentation: ActivityRunPresentation,
  rows: number,
): void {
  if (rows === 0) return;
  const bucket = buckets.get(presentation.key);
  if (bucket) bucket.count += rows;
  else buckets.set(presentation.key, { ...presentation, count: rows });
}

function isFailedStatus(status: Item['status']): boolean {
  // `declined` is a user decision, not a failure; `killed` and `errored`
  // are outcomes the user did not choose and a collapsed run must not hide.
  return status === 'errored' || status === 'killed';
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
 *
 * `stub` is what the pane knows about the members it does NOT hold
 * (docs/architecture/timeline-window-pages.md §4, §6): shed rows, which
 * are classified here like any other member, and the server's aggregate
 * of the rest. Null for a run the pane holds whole, which is every run
 * with no stub. Members from all three sources fold into one header, so a
 * collapsed run reports its whole self whatever part of it is loaded.
 */
export function activityRunSummary(
  items: readonly Item[],
  provider: ProviderID | null | undefined,
  stub: ActivityRunStubFacts | null = null,
): ActivityRunSummary {
  const shed = stub?.shed ?? EMPTY_SHED;
  const presentIds = new Set(items.map((item) => item.id));
  for (const row of shed) presentIds.add(row.id);
  const completedCallIds = new Set<string>();
  for (const item of items) {
    if (item.kind === 'tool_completion' && item.completionOf) {
      completedCallIds.add(item.completionOf);
    }
  }
  for (const row of shed) {
    if (row.kind === 'tool_completion' && row.completionOf !== '') {
      completedCallIds.add(row.completionOf);
    }
  }
  // Members the pane does not hold whose completion it DOES hold (§4).
  // The held completion pairs with them and counts zero, exactly as it
  // would if both rows were loaded.
  for (const id of stub?.unshippedPairedLaunchIds ?? []) presentIds.add(id);
  // And the mirror: held launches whose completion the pane does not
  // hold. Their status is superseded exactly as if the completion were
  // loaded; a detached launch would otherwise read as running forever.
  for (const id of stub?.shippedSupersededLaunchIds ?? []) completedCallIds.add(id);
  // A run can hold hundreds of repeated Bash/Edit rows and this summary
  // re-evaluates on streaming deltas. Classify each distinct source name once
  // per pass; a module-level cache would be unbounded by provider input.
  const presentationCache = new Map<string, ActivityRunPresentation>();
  const buckets = new Map<string, ActivityRunCountEntry>();
  let total = 0;
  let hasFailure = false;
  let runningLabel: string | null = null;

  // Oldest first, so "last running wins" holds across the whole run: the
  // stub's `runningBefore` edge, then the shed rows, then the loaded
  // ones, then its `runningAfter` edge.
  if (stub?.runningBefore) {
    runningLabel = presentationForGroupKey(stub.runningBefore, provider, presentationCache).label;
  }
  for (const row of shed) {
    if (!completedCallIds.has(row.id)) {
      if (isFailedStatus(row.status as Item['status'])) hasFailure = true;
      if (isRunningStatus(row.status as Item['status'])) {
        runningLabel = presentationForGroupKey(row, provider, presentationCache).label;
      }
    }
    // A shed completion pairs with a call the run still holds anywhere —
    // loaded, shed, or named by the stub's pairing list.
    if (row.kind === 'tool_completion' && row.completionOf !== ''
      && presentIds.has(row.completionOf)) continue;
    // `fileRows` was resolved when the row was shed, from the payload
    // blob the record deliberately does not retain.
    addRows(buckets, presentationForGroupKey(row, provider, presentationCache), row.fileRows);
    total += row.fileRows;
  }

  for (const item of items) {
    // A completion supersedes its immutable call record. Detached agent
    // launches deliberately remain `running` forever in canonical history,
    // so considering both statuses would keep a false running indicator after
    // the completion landed.
    if (!completedCallIds.has(item.id)) {
      if (isFailedStatus(item.status)) hasFailure = true;
      // Last one wins: the newest active row is what the user wants named.
      if (isRunningStatus(item.status)) {
        runningLabel = activityRunPresentation(item, provider, presentationCache).label;
      }
    }

    if (item.kind === 'tool_completion') {
      const callId = item.completionOf;
      if (callId && presentIds.has(callId)) continue;
    }
    const displayRowCount = fileChangeDisplayRowCount(item);
    total += displayRowCount;
    addRows(buckets, activityRunPresentation(item, provider, presentationCache), displayRowCount);
  }

  // Finally the members the pane holds neither as rows nor as shed
  // copies: the server's aggregate of the same rule over the same rows.
  for (const group of stub?.unshippedGroups ?? []) {
    total += group.rows;
    addRows(buckets, presentationForGroupKey(group, provider, presentationCache), group.rows);
  }
  if (stub?.unshippedFailed) hasFailure = true;
  if (stub?.runningAfter) {
    runningLabel = presentationForGroupKey(stub.runningAfter, provider, presentationCache).label;
  }

  const entries = [...buckets.values()]
    .sort((a, b) => {
      // Ambient rows last regardless of count — a reader scanning a header
      // wants the tools first. Notifications before thinking: they are
      // events, thinking is background hum.
      const rank = (e: ActivityRunCountEntry): number =>
        e.isThinking ? 2 : e.key === NOTIFICATION_PRESENTATION.key ? 1 : 0;
      const aRank = rank(a);
      const bRank = rank(b);
      if (aRank !== bRank) return aRank - bRank;
      if (a.count !== b.count) return b.count - a.count;
      return a.label.localeCompare(b.label);
    });
  for (const entry of entries) {
    if (entry.key === NOTIFICATION_PRESENTATION.key && entry.count > 1) {
      entry.label = 'notifications';
    }
  }

  return { counts: { entries, total }, hasFailure, runningLabel };
}
