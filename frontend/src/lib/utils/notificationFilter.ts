// A `task_notification` row is hidden once a COMPLETED lifecycle
// sibling with the same task_id exists. The bell's text ("Background
// command … completed (exit code 0)") is the CLI's formulaic
// restatement of facts the completion card already shows — description
// and exit code — so rendering both prints one completion twice, and
// the common agentic wait pattern (bg Bash + a blocking TaskOutput)
// made that the NORMAL case: the TaskOutput drain writes the sibling
// before the bell arrives, so an absorption-only rule (caption stamped
// on the sibling's first write) left the bell visible on every waited
// background command (user ruling 2026-08-22). Existence of the
// completed sibling is therefore the whole hide predicate. Nothing is
// lost durably: the notification row stays in SQLite (the backend
// persists it for the load-bearing `output_file` enrichment side
// effect in `internal/triage/background_task_notifications.go`) and
// the caption still renders on the card when the write order let
// triage stamp it — this filter only suppresses rendering.
//
// Only a COMPLETED sibling hides. A running launch keeps its bell, and
// so do errored/killed ones — the explicit failure ping is wanted.
//
// LOAD-BEARING ASSUMPTION: the completed sibling RENDERS, in place, at
// the completion point. This filter deletes the only other row that says
// "the task finished", so anything downstream that folds the sibling
// away — the subagent grouping folds it onto the launch card as the
// card's status source — must keep the sibling's own row as well. It
// did not, once (2026-08-22): the fold also dropped the sibling from the
// node array, and a finished agent left no trace in the main transcript.
// `backgroundCompletionVisibility.test.ts` runs this filter and the
// grouping in production order and counts rows; keep it green.
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
// depend on the launch row still being in the rendered window.
//
// Operates on the flat `pane.items` array before subagent grouping so a
// hidden notification never enters the rendered tree, including when the
// notification's `parentId` would have placed it inside a SubagentGroup.

import type { Item } from '../types/models';
import { extractClaudeTaskID, isClaudeWatchTaskNotification } from './claudeTaskMeta';

export function filterRedundantNotifications(items: readonly Item[]): readonly Item[] {
  // Hot path: no notifications → nothing to filter, return the original
  // array reference so downstream `$derived` chains see no change.
  if (!items.some((it) => it.kind === 'notification')) return items;

  const completedTaskIDs = new Set<string>();
  for (const it of items) {
    const isCompletedLifecycle =
      it.kind === 'tool_completion' ||
      (it.kind === 'tool_call' && it.status === 'completed');
    if (!isCompletedLifecycle) continue;
    const id = extractClaudeTaskID(it);
    if (id) completedTaskIDs.add(id);
  }
  if (completedTaskIDs.size === 0) return items;

  const out: Item[] = [];
  for (const it of items) {
    if (it.kind === 'notification' && !isClaudeWatchTaskNotification(it)) {
      const id = extractClaudeTaskID(it);
      if (id && completedTaskIDs.has(id)) continue;
    }
    out.push(it);
  }
  return out;
}
