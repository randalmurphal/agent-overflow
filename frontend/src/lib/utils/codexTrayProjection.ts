import type { Item } from '../types/models';
import { parseJsonObject } from './parseJsonObject';

// Tray-only metadata decorated onto a read-time Codex launch copy. Keep these
// values mirrored with internal/store/subagent_items.go; mirror_pins_test.go
// enforces the cross-language contract.
export const CODEX_LATEST_TOOL_META = {
  summary: 'subagentLatestToolSummary',
  turnIndex: 'subagentLatestToolTurnIndex',
  itemIndex: 'subagentLatestToolItemIndex',
} as const;

/**
 * Projects a Codex agent's live child tool calls onto its tray row between
 * tray reads, so the row's activity line follows the agent without a
 * `ListLiveBackgroundTasks` round trip per tool call.
 */
export interface CodexLatestToolProjection {
  /** Index a tray snapshot. Call with every wholesale snapshot write. */
  reset(items: readonly Item[]): void;
  /**
   * `items` with `tool` projected onto its parent agent's row, or `items`
   * itself when the tool changes nothing. `items` must be the last snapshot
   * passed to `reset` or returned here. Reads only the parent's row, and
   * copies the snapshot only when that row changes.
   */
  apply(items: Item[], tool: Item): Item[];
}

function metaIndex(value: unknown): number {
  return typeof value === 'number' ? value : -1;
}

export function createCodexLatestToolProjection(): CodexLatestToolProjection {
  // Position of each running Codex agent row in the snapshot. The last
  // running row for an id wins, the same row `deriveTrayTasks` shows.
  let rows = new Map<string, number>();
  return {
    reset(items) {
      rows = new Map();
      items.forEach((item, index) => {
        if (item.toolName === 'collab_agent' && item.status === 'running' && !item.completionOf) {
          rows.set(item.id, index);
        }
      });
    },
    apply(items, tool) {
      const parentId = tool.parentId?.trim();
      if (!parentId || tool.toolName === 'collab_agent') return items;
      const index = rows.get(parentId);
      if (index === undefined) return items;
      const summary = tool.summary.trim();
      if (!summary) return items;
      const row = items[index];
      const parsedMeta = parseJsonObject(row.meta);
      if (row.meta?.trim() && parsedMeta === null) {
        console.error(`ActivityRail: malformed Codex launch meta for ${row.id}`);
        return items;
      }
      const meta = parsedMeta ?? {};
      const currentTurn = metaIndex(meta[CODEX_LATEST_TOOL_META.turnIndex]);
      const currentItem = metaIndex(meta[CODEX_LATEST_TOOL_META.itemIndex]);
      if (tool.turnIndex < currentTurn || (tool.turnIndex === currentTurn && tool.itemIndex < currentItem)) {
        return items;
      }
      if (
        meta[CODEX_LATEST_TOOL_META.summary] === summary
        && tool.turnIndex === currentTurn
        && tool.itemIndex === currentItem
      ) return items;
      const next = items.slice();
      next[index] = {
        ...row,
        meta: JSON.stringify({
          ...meta,
          [CODEX_LATEST_TOOL_META.summary]: summary,
          [CODEX_LATEST_TOOL_META.turnIndex]: tool.turnIndex,
          [CODEX_LATEST_TOOL_META.itemIndex]: tool.itemIndex,
        }),
      };
      return next;
    },
  };
}
