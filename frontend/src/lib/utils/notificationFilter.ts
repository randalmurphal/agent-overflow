// A `task_notification` whose task already shows a terminal lifecycle row
// is duplicate UI noise — the lifecycle row IS the in-app completion
// signal. The original `notification` row stays in SQLite (the backend
// persists it for the load-bearing `output_file` enrichment side effect
// in `internal/triage/background_task_notifications.go`); this filter
// only suppresses rendering.
//
// Operates on the flat `pane.items` array before subagent grouping so a
// hidden notification never enters the rendered tree, including when the
// notification's `parentId` would have placed it inside a SubagentGroup.

import type { Item } from '../types/models';
import { extractClaudeTaskID } from './claudeTaskMeta';

export function filterRedundantNotifications(items: readonly Item[]): readonly Item[] {
  // Hot path: no notifications → nothing to filter, return the original
  // array reference so downstream `$derived` chains see no change.
  if (!items.some((it) => it.kind === 'notification')) return items;

  const completedTaskIds = new Set<string>();
  for (const it of items) {
    const isCompletedLifecycle =
      it.kind === 'tool_completion' ||
      (it.kind === 'tool_call' && it.status === 'completed');
    if (!isCompletedLifecycle) continue;
    const id = extractClaudeTaskID(it);
    if (id) completedTaskIds.add(id);
  }
  if (completedTaskIds.size === 0) return items;

  const out: Item[] = [];
  for (const it of items) {
    if (it.kind === 'notification') {
      const id = extractClaudeTaskID(it);
      if (id && completedTaskIds.has(id)) continue;
    }
    out.push(it);
  }
  return out;
}
