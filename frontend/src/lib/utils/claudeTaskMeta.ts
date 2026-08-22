import type { Item } from '../types/models';
import { parseJsonObject } from './parseJsonObject';

/**
 * Extract the Claude `task_id` from an item's `meta` JSON blob.
 * Triage stamps this onto launch/completion/notification rows so the
 * frontend can correlate task lifecycle UI without reparsing the same
 * metadata string at every call site.
 */
export function extractClaudeTaskID(item: Item): string | null {
  const id = parseJsonObject(item.meta)?.task_id;
  return typeof id === 'string' && id.length > 0 ? id : null;
}

/**
 * Whether a `notification` row records an event of a WATCH task
 * (Claude's Monitor — claude-wire.md §E7) rather than the single
 * completion bell of an ordinary background task.
 *
 * Triage stamps `meta.watch_task` at write time from the launch row, and
 * only ever as `true` — an absent key means "not a watch task", which is
 * also what every row persisted before the field existed says.
 */
export function isClaudeWatchTaskNotification(item: Item): boolean {
  return parseJsonObject(item.meta)?.watch_task === true;
}

/**
 * The summary text a `system/task_notification` carried, as stamped onto
 * the `tool_completion` sibling it explains. Present only when the wire
 * supplied one, and only on rows written since triage started carrying
 * it — the caller renders nothing when it is null.
 *
 * Never for a row whose notification carried an `output_file`: there the
 * "summary" IS the task's full output (an async Agent's final report),
 * which already lives on the row's expandable payload — a caption would
 * dump the whole report inline (2026-08-22, "Test nested agent
 * spawning"). Triage no longer stamps a caption on those siblings; this
 * guard also retro-hides rows persisted before that fix.
 */
export function extractNotificationSummary(item: Item): string | null {
  const meta = parseJsonObject(item.meta);
  const summary = meta?.notification_summary;
  if (typeof summary !== 'string') return null;
  if (typeof meta?.notification_output_state === 'string' && meta.notification_output_state !== '')
    return null;
  const trimmed = summary.trim();
  return trimmed.length > 0 ? trimmed : null;
}
