// Pure derivation helpers for the activity rail's Background segment.
// Kept in utils/ so the grouping/pruning/sort logic can be unit-tested
// without mounting the component and so the .svelte file stays under
// the 300-line ceiling.

import type { ProviderBackgroundStop } from '../providers/catalog';
import type { Item } from '../types/models';
import { extractClaudeTaskID } from './claudeTaskMeta';
import { extractCodexProcessID } from './codexProcessMeta';
import {
  isCodexSubagentLaunchItem,
  NO_LOADED_SUBAGENT_CHILDREN,
  subagentLaunchInfo,
  type SubagentLaunchInfo,
} from './subagentLaunch';

export { extractClaudeTaskID } from './claudeTaskMeta';
export { extractCodexProcessID } from './codexProcessMeta';

export interface TrayTask {
  /** Stable id used for the row key and scroll-to-item request. */
  rowId: string;
  /** Primary item the row reads summary/timing from — the launch when
   * we have one, otherwise the completion. */
  anchor: Item;
  /** The launch item, if it's still in the backend's live set. */
  launch: Item | null;
  /** The completion item, if one has landed for this launch. */
  completion: Item | null;
  /** Resolved status for the badge + pulse. */
  status: 'running' | 'completed' | 'errored' | 'declined' | 'killed';
  /** ms since the launch started; null when we only have a completion
   * (no meaningful start time to count from). */
  elapsedMs: number | null;
  /** Ancestry depth WITHIN the tray set: how many tray rows sit on this
   * row's parent chain. 0 for a background root — including a background
   * launch under a FOREGROUND agent, whose parent is deliberately not in
   * the set (accepted deviation: the tray lists backgrounded ancestry,
   * and a foreground parent is the timeline's to show). */
  depth: number;
}

/**
 * The provider-neutral launch identity for a tray row, or null for a
 * plain command row. Uses the cold-thread context (no loaded rows): a
 * tray entry is a single row read in isolation, so forked-Skill
 * detection rests on its meta signals — exactly what
 * NO_LOADED_SUBAGENT_CHILDREN documents.
 */
export function trayTaskAgentInfo(task: TrayTask): SubagentLaunchInfo | null {
  const launch = task.launch ?? task.completion;
  if (!launch) return null;
  return subagentLaunchInfo(launch, NO_LOADED_SUBAGENT_CHILDREN);
}

// The backend exposes four terminal statuses for a completion row:
// `completed`, `errored`, `declined`, and `killed`. The tray used to
// collapse `declined` into `completed` (green ✓) even though the rest
// of the UI (ToolDecisionChip) renders declined as error-colored; we
// map each through faithfully so the affordance matches. `killed` is
// a user-initiated stop — distinct from `errored` (which carries the
// provider's failure message) and gets its own gray "Stopped" badge.
export function completionStatusFor(completion: Item): TrayTask['status'] {
  if (completion.status === 'errored') return 'errored';
  if (completion.status === 'declined') return 'declined';
  if (completion.status === 'killed') return 'killed';
  return 'completed';
}

/**
 * Codex subagent rows are detected from the normalized spawn-agent
 * launch metadata. They represent child threads with no client-side kill
 * path. The Stop-all button must hide when the tray only contains these,
 * because neither StopClaudeTask nor CleanCodexBackgroundTerminals can
 * touch them.
 */
export function isCodexSubagentTask(task: TrayTask): boolean {
  const item = task.launch ?? task.completion;
  return item ? isCodexSubagentLaunchItem(item) : false;
}

/**
 * Codex's stop primitives (per-row `TerminateCodexBackgroundTerminal`
 * and thread-wide `CleanCodexBackgroundTerminals`) both act only on
 * yielded unifiedExec PTYs. Pending foreground commands are visible in
 * the same running tray, but they are not background terminals yet, so
 * offering a stop for them would promise a kill primitive the backend
 * does not have.
 */
export function isCodexStoppableTask(task: TrayTask): boolean {
  if (isCodexSubagentTask(task)) return false;
  return task.launch?.isBackground === true;
}

/**
 * Resolve the id a row's Stop button would target, or null when the row
 * has no per-row stop at all. The id namespace is per provider — a
 * Claude task id for `claude-task`, a Codex PTY process id for
 * `codex-background-terminals` — and the caller pairs it with the
 * matching binding.
 *
 * Only running rows with a live launch are stoppable: a completed row
 * has nothing to kill, and a launch that has already been pruned from
 * the tray's source list carries no id to kill it by.
 */
export function trayRowStopTarget(
  task: TrayTask,
  backgroundStop: ProviderBackgroundStop,
): string | null {
  if (task.status !== 'running' || task.launch === null) return null;
  switch (backgroundStop) {
    case 'claude-task':
      return extractClaudeTaskID(task.launch);
    case 'codex-background-terminals':
      // Same gate as Stop-all, plus the id: a spawned collab-agent child
      // and a not-yet-yielded foreground command are both untouchable by
      // the terminate RPC, and the wire may not have named the process
      // yet on a row that just started.
      return isCodexStoppableTask(task) ? extractCodexProcessID(task.launch) : null;
    case 'none':
      return null;
  }
}

/**
 * Primary visible label for a tray row. Launch summary wins (the
 * provider adapters already fill it with the tool name); completion
 * summary is the fallback for orphan completions; "Tool" is the last
 * resort so the row never renders nameless.
 */
export function trayTaskLabel(task: TrayTask): string {
  const fromLaunch = (task.launch?.summary ?? '').trim();
  if (fromLaunch) return fromLaunch;
  const fromCompletion = (task.completion?.summary ?? '').trim();
  if (fromCompletion) return fromCompletion;
  return 'Tool';
}

/** Status label rendered next to the glyph on a tray row. */
export function statusLabel(status: TrayTask['status']): string {
  if (status === 'running') return 'running';
  if (status === 'errored') return 'failed';
  if (status === 'declined') return 'declined';
  if (status === 'killed') return 'stopped';
  return '';
}

/**
 * Tailwind text-color class for the status span. `declined` shares
 * the error palette with ToolDecisionChip — a user-declined tool run
 * is not a success, and the rest of the UI already colors it the
 * same way a tool failure is colored. `killed` lands on muted-gray
 * text so it reads as a user-initiated terminal, distinct from the
 * red of an actual failure.
 */
export function statusClass(status: TrayTask['status']): string {
  if (status === 'running') return 'text-accent';
  if (status === 'errored' || status === 'declined') return 'text-error';
  if (status === 'killed') return 'text-text-secondary';
  return 'rounded bg-success/10 px-1 py-px text-success';
}

/**
 * Human-readable elapsed-time label — `12s` / `3m 7s` / `1h 12m 5s`.
 * Tray rows call this once per 1s tick against the task's live
 * elapsedMs; the formatting stays frontend-rendered because the Go
 * side computes raw deltas without locale-aware formatting.
 */
export function formatElapsed(ms: number): string {
  const seconds = Math.floor(ms / 1000);
  if (seconds < 60) return `${seconds}s`;
  const minutes = Math.floor(seconds / 60);
  const remSec = seconds % 60;
  if (minutes < 60) return `${minutes}m ${remSec}s`;
  const hours = Math.floor(minutes / 60);
  const remMin = minutes % 60;
  return `${hours}h ${remMin}m ${remSec}s`;
}

/**
 * Group each running launch with its tool_completion sibling, prune
 * pairs whose completion has aged past `retentionMs`, and sort
 * oldest created first to match the composer-attached tray's stable
 * reading order.
 *
 * Persisted background rows arrive as independent immutable rows: the
 * launch stays `status='running'` forever (spec invariant — see
 * docs/architecture/chat-rewrite.md "Background tray"), and the
 * completion is a separate row linked via `completionOf`. The tray
 * can't treat "launch.status === 'running'" as liveness for those rows
 * — it must treat a (launch, completion) pair as a single logical task
 * and drop BOTH once the completion ages past the retention window.
 * Otherwise the launch would re-render as "Running" forever after
 * retention elapsed. Pending Codex foreground commands are transient
 * launches with no completion sibling until they either complete
 * quickly or become backgrounded.
 */
export function deriveTrayTasks(
  items: readonly Item[],
  now: number,
  retentionMs: number,
): TrayTask[] {
  interface Bucket {
    launch: Item | null;
    completion: Item | null;
  }
  const buckets = new Map<string, Bucket>();
  const bucketFor = (key: string): Bucket => {
    let b = buckets.get(key);
    if (!b) {
      b = { launch: null, completion: null };
      buckets.set(key, b);
    }
    return b;
  };

  for (const item of items) {
    if (item.completionOf) {
      const b = bucketFor(item.completionOf);
      // The backend upserts the same completion id in place, so a
      // duplicate is rare; the createdAt comparison is defensive
      // against any out-of-order delivery.
      if (!b.completion || b.completion.createdAt < item.createdAt) {
        b.completion = item;
      }
    } else if (item.status === 'running') {
      bucketFor(item.id).launch = item;
    }
  }

  const out: TrayTask[] = [];
  for (const { launch, completion } of buckets.values()) {
    // Drop the whole pair once the completion ages out. A completion
    // without a launch (launch already pruned from the source list)
    // still renders during the window so the user sees the final
    // state land.
    if (
      completion &&
      now - completion.createdAt >= retentionMs
    ) continue;
    const anchor = launch ?? completion;
    if (!anchor) continue;

    const status: TrayTask['status'] = completion
      ? completionStatusFor(completion)
      : 'running';

    out.push({
      rowId: anchor.id,
      anchor,
      launch,
      completion,
      status,
      // Only the launch has a meaningful "started at" timestamp. For
      // orphan completions (no launch in the list) counting elapsed
      // time from the completion's createdAt would misleadingly show
      // "0s" for a task that actually ran for minutes — so we omit
      // the label entirely in that case.
      elapsedMs: launch ? Math.max(0, now - launch.createdAt) : null,
      depth: 0,
    });
  }

  out.sort((a, b) => a.anchor.createdAt - b.anchor.createdAt);

  // Tree order + depth: children render under their parent, indented by
  // how many TRAY rows sit on their parent chain (walked through the
  // full input set — the backend supplies every intermediate agent
  // launch between a background root and a nested launch, so the chain
  // never dead-ends inside the set).
  const parentById = new Map<string, string>();
  for (const item of items) {
    if (item.parentId) parentById.set(item.id, item.parentId);
  }
  const taskByRowId = new Map(out.map((t) => [t.rowId, t] as const));
  const childrenByRow = new Map<string, TrayTask[]>();
  const roots: TrayTask[] = [];
  for (const task of out) {
    let hops = 0;
    let nearestTrayAncestor: string | null = null;
    for (
      let pid = parentById.get(task.rowId);
      pid !== undefined && hops < 64;
      pid = parentById.get(pid), hops++
    ) {
      if (taskByRowId.has(pid)) {
        nearestTrayAncestor = pid;
        break;
      }
    }
    if (nearestTrayAncestor === null) {
      roots.push(task);
    } else {
      let bucket = childrenByRow.get(nearestTrayAncestor);
      if (!bucket) childrenByRow.set(nearestTrayAncestor, (bucket = []));
      bucket.push(task);
    }
  }
  const ordered: TrayTask[] = [];
  const place = (task: TrayTask, depth: number): void => {
    task.depth = depth;
    ordered.push(task);
    for (const child of childrenByRow.get(task.rowId) ?? []) place(child, depth + 1);
  };
  for (const root of roots) place(root, 0);
  return ordered;
}
