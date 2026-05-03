// Pure derivation helpers for BackgroundTaskTray. Kept in utils/ so the
// grouping/pruning/sort logic can be unit-tested without mounting the
// component and so the .svelte file stays under the 300-line ceiling.

import type { Item } from '../types/models';
import { isCodexSubagentLaunchItem } from './subagentLaunch';

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
 * Extract the Claude `task_id` from a launch item's `meta` JSON blob.
 * The Claude parser stamps `task_id` onto `items.meta` via
 * `mergeItemMetaTaskID` in triage — returned verbatim here so the tray
 * can hand it to `StopClaudeTask(threadID, taskID)`.
 *
 * Returns null when the meta string is missing, unparseable, or does
 * not carry a non-empty `task_id` field. Codex launches never carry a
 * task_id; this is the signal the tray uses to decide whether to
 * render a per-row Stop button.
 */
export function extractClaudeTaskID(item: Item): string | null {
  const raw = item.meta;
  if (!raw) return null;
  try {
    const parsed = JSON.parse(raw) as { task_id?: unknown };
    const id = parsed?.task_id;
    if (typeof id === 'string' && id.length > 0) return id;
    return null;
  } catch {
    return null;
  }
}

/**
 * Codex subagent rows are detected from the normalized spawn-agent
 * launch metadata. They represent child threads with no client-side kill path. The Stop-all button
 * must hide when the tray only contains these, because neither
 * StopClaudeTask nor CleanCodexBackgroundTerminals can touch them.
 */
export function isCodexSubagentTask(task: TrayTask): boolean {
  const item = task.launch ?? task.completion;
  return item ? isCodexSubagentLaunchItem(item) : false;
}

/**
 * Codex can only stop yielded unifiedExec PTYs via
 * CleanCodexBackgroundTerminals. Pending foreground commands are
 * visible in the same running tray, but they are not background
 * terminals yet, so rendering Stop-all for them would promise a kill
 * primitive the backend does not have.
 */
export function isCodexStoppableTask(task: TrayTask): boolean {
  if (isCodexSubagentTask(task)) return false;
  return task.launch?.isBackground === true;
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
 * Human-readable elapsed-time label — `12s` / `3m 7s` / `1h 12m`.
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
  return `${hours}h ${remMin}m`;
}

/**
 * Group each running launch with its tool_completion sibling, prune
 * pairs whose completion has aged past `retentionMs`, and sort oldest
 * created first to match the composer-attached tray's stable reading
 * order.
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
    if (completion && now - completion.createdAt >= retentionMs) continue;
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
    });
  }

  return out.sort((a, b) => a.anchor.createdAt - b.anchor.createdAt);
}
