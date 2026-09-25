// Hides a plain background COMMAND's bell once its completion renders, and
// never touches an agent's rows.
//
// A command's `task_notification` bell ("Background command … completed
// (exit code 0)") is the CLI's formulaic restatement of facts the
// completion card already shows, so rendering both prints one completion
// twice. The common agentic wait pattern (bg Bash + a blocking TaskOutput)
// writes the sibling before the bell arrives, so existence of a rendered
// lifecycle row with the bell's task_id that says the command ended is the
// whole hide predicate (user ruling 2026-08-22): a completion sibling
// whatever its status, or a tool call once completed. A running, errored
// or killed tool call keeps its bell as the explicit failure ping. Nothing
// is lost durably: the row stays in SQLite, and this filter only
// suppresses rendering.
//
// An agent's rows (the Agent and Task tools and a §E6 resume carrier's
// SendMessage) are never hidden and never hide anything: every stop of an
// agent is its own completion sibling and card (docs/specs/
// agent-visibility.md, §Agent runs and stops), triage writes an agent no
// bell, and an agent bell older builds left with no sibling to cover it is
// the only record of that report.
//
// LOAD-BEARING ASSUMPTION: the completion sibling RENDERS, in place, at
// the completion point. This filter deletes the only other row that says
// "the task finished". `backgroundCompletionVisibility.test.ts` runs this
// filter and the grouping in production order and counts rows; keep it
// green.
//
// A WATCH task's notifications (Claude's Monitor, claude-wire.md §E7) are
// exempt: a Monitor fires one per output event of the stream it watches,
// so those rows are its history, and its terminal lifecycle row means only
// "the stream ended". Triage stamps `meta.watch_task` onto each at write
// time so the decision does not depend on the launch row being in the
// rendered window.
//
// Operates on the flat `pane.items` array before subagent grouping so a
// hidden notification never enters the rendered tree, including when the
// notification's `parentId` would have placed it inside a SubagentGroup.

import type { Item } from '../types/models';
import { extractClaudeTaskID, isClaudeWatchTaskNotification } from './claudeTaskMeta';

/** An agent's row, by the tool the row records. */
function isAgentRow(item: Item): boolean {
  const tool = (item.toolName ?? '').trim();
  return tool === 'Agent' || tool === 'Task' || tool === 'SendMessage';
}

function isCommandBell(item: Item): boolean {
  return item.kind === 'notification' && !isAgentRow(item);
}

export function filterRedundantNotifications(
  items: readonly Item[],
  rendersLifecycle: (item: Item) => boolean = () => true,
): readonly Item[] {
  // Hot path: no command bells → nothing to filter, return the original
  // array reference so downstream `$derived` chains see no change.
  if (!items.some(isCommandBell)) return items;

  const completedTaskIDs = new Set<string>();
  for (const it of items) {
    const isCompletedLifecycle =
      it.kind === 'tool_completion' ||
      (it.kind === 'tool_call' && it.status === 'completed');
    if (!isCompletedLifecycle || isAgentRow(it) || !rendersLifecycle(it)) continue;
    const id = extractClaudeTaskID(it);
    if (id) completedTaskIDs.add(id);
  }
  if (completedTaskIDs.size === 0) return items;

  const out: Item[] = [];
  for (const it of items) {
    if (isCommandBell(it) && !isClaudeWatchTaskNotification(it)) {
      const id = extractClaudeTaskID(it);
      if (id && completedTaskIDs.has(id)) continue;
    }
    out.push(it);
  }
  return out;
}
