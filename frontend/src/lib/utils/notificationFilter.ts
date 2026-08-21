// A `task_notification` row is hidden only when its completion sibling
// has ABSORBED it — the sibling carries the notification's summary as a
// caption (`meta.notification_summary`, stamped by triage on the
// sibling's first write), or the sibling's own summary already says the
// same thing. A completion that merely EXISTS is not enough: on the
// sibling-first write order (a TaskOutput drain created the row before
// the bell arrived) the caption misses its one chance — a mounted card
// must never grow a line — and hiding the notification there would
// silently drop the summary text the model can see. The original
// `notification` row always stays in SQLite (the backend persists it for
// the load-bearing `output_file` enrichment side effect in
// `internal/triage/background_task_notifications.go`); this filter only
// suppresses rendering.
//
// A WATCH task (Claude's Monitor — claude-wire.md §E7) is the one
// exception, and it is not a special case so much as a different shape:
// a Monitor fires one notification per output event of the stream it
// watches, so those rows are the interim history rather than one
// redundant bell, and its terminal lifecycle row means only "the stream
// ended". Suppressing them on that signal erased the entire history at
// the exact moment the run finished. Triage stamps `meta.watch_task`
// onto each notification at write time (copied from the launch row,
// which the keep-running flip marks) precisely so this decision does not
// depend on the launch row still being in the rendered window — and
// never stamps a caption for a watch task, so the absorption test below
// is naturally false for them too; the kind check is belt on top.
//
// Operates on the flat `pane.items` array before subagent grouping so a
// hidden notification never enters the rendered tree, including when the
// notification's `parentId` would have placed it inside a SubagentGroup.

import type { Item } from '../types/models';
import {
  extractClaudeTaskID,
  extractNotificationSummary,
  isClaudeWatchTaskNotification,
} from './claudeTaskMeta';

interface CompletionAbsorption {
  /** The sibling carries a `notification_summary` caption. */
  captioned: boolean;
  /** Trimmed completion summaries, for the equal-text fallback. */
  summaries: Set<string>;
}

export function filterRedundantNotifications(items: readonly Item[]): readonly Item[] {
  // Hot path: no notifications → nothing to filter, return the original
  // array reference so downstream `$derived` chains see no change.
  if (!items.some((it) => it.kind === 'notification')) return items;

  const completions = new Map<string, CompletionAbsorption>();
  for (const it of items) {
    const isCompletedLifecycle =
      it.kind === 'tool_completion' ||
      (it.kind === 'tool_call' && it.status === 'completed');
    if (!isCompletedLifecycle) continue;
    const id = extractClaudeTaskID(it);
    if (!id) continue;
    let entry = completions.get(id);
    if (!entry) {
      entry = { captioned: false, summaries: new Set() };
      completions.set(id, entry);
    }
    if (extractNotificationSummary(it) !== null) entry.captioned = true;
    const summary = it.summary?.trim();
    if (summary) entry.summaries.add(summary);
  }
  if (completions.size === 0) return items;

  const out: Item[] = [];
  for (const it of items) {
    if (it.kind === 'notification' && !isClaudeWatchTaskNotification(it)) {
      const id = extractClaudeTaskID(it);
      const entry = id ? completions.get(id) : undefined;
      if (entry && (entry.captioned || entry.summaries.has(it.summary?.trim() ?? ''))) {
        continue;
      }
    }
    out.push(it);
  }
  return out;
}
