import type { Item } from '../types/models';
import { parseJsonObject } from './parseJsonObject';

// The tray's latest-tool decoration on an agent row: the summary and
// position of the agent's newest direct tool call. The store writes it on
// the rows the tray reads (Codex agent rows today). Keep these values
// mirrored with internal/store/subagent_items.go; mirror_pins_test.go
// enforces the cross-language contract.
export const TRAY_LATEST_TOOL_META = {
  summary: 'subagentLatestToolSummary',
  turnIndex: 'subagentLatestToolTurnIndex',
  itemIndex: 'subagentLatestToolItemIndex',
} as const;

interface LatestTool {
  summary: string;
  turnIndex: number;
  itemIndex: number;
}

/**
 * Keeps a listed launch's latest-tool decoration current between tray
 * reads from the launch's own re-pushes, so its activity line follows the
 * agent without a `ListLiveBackgroundTasks` round trip per push. The tray
 * reads no child rows: a Codex agent's line comes from the list read its
 * tool calls nudge.
 *
 * `applyPushedLaunch` takes `items`, which must be the last snapshot passed
 * to `reset` or returned by it, and returns it unchanged when the push
 * moves nothing. It reads only the target row and copies the snapshot only
 * when that row changes.
 */
export interface TrayLatestToolProjection {
  /** Index a tray snapshot. Call with every wholesale snapshot write. */
  reset(items: readonly Item[]): void;
  /**
   * A re-pushed launch's own decoration, carried onto its row. A push
   * without the decoration keeps the row's current value.
   */
  applyPushedLaunch(items: Item[], launch: Item): Item[];
}

function metaIndex(value: unknown): number {
  return typeof value === 'number' ? value : -1;
}

function pushedLatestTool(launch: Item): LatestTool | null {
  const meta = parseJsonObject(launch.meta);
  const summary = meta?.[TRAY_LATEST_TOOL_META.summary];
  const turnIndex = meta?.[TRAY_LATEST_TOOL_META.turnIndex];
  const itemIndex = meta?.[TRAY_LATEST_TOOL_META.itemIndex];
  if (typeof summary !== 'string' || typeof turnIndex !== 'number' || typeof itemIndex !== 'number') return null;
  const trimmed = summary.trim();
  return trimmed ? { summary: trimmed, turnIndex, itemIndex } : null;
}

export function createTrayLatestToolProjection(): TrayLatestToolProjection {
  // Position of each running launch row in the snapshot. The last running
  // row for an id wins, the same row `deriveTrayTasks` shows.
  let rows = new Map<string, number>();

  function project(items: Item[], index: number, row: Item, latest: LatestTool): Item[] {
    const parsedMeta = parseJsonObject(row.meta);
    if (row.meta?.trim() && parsedMeta === null) {
      console.error(`ActivityRail: malformed tray row meta for ${row.id}`);
      return items;
    }
    const meta = parsedMeta ?? {};
    const currentTurn = metaIndex(meta[TRAY_LATEST_TOOL_META.turnIndex]);
    const currentItem = metaIndex(meta[TRAY_LATEST_TOOL_META.itemIndex]);
    if (latest.turnIndex < currentTurn || (latest.turnIndex === currentTurn && latest.itemIndex < currentItem)) {
      return items;
    }
    if (
      meta[TRAY_LATEST_TOOL_META.summary] === latest.summary
      && latest.turnIndex === currentTurn
      && latest.itemIndex === currentItem
    ) return items;
    const next = items.slice();
    next[index] = {
      ...row,
      meta: JSON.stringify({
        ...meta,
        [TRAY_LATEST_TOOL_META.summary]: latest.summary,
        [TRAY_LATEST_TOOL_META.turnIndex]: latest.turnIndex,
        [TRAY_LATEST_TOOL_META.itemIndex]: latest.itemIndex,
      }),
    };
    return next;
  }

  return {
    reset(items) {
      rows = new Map();
      items.forEach((item, index) => {
        if (item.status === 'running' && !item.completionOf) rows.set(item.id, index);
      });
    },
    applyPushedLaunch(items, launch) {
      const index = rows.get(launch.id);
      if (index === undefined) return items;
      const latest = pushedLatestTool(launch);
      return latest ? project(items, index, items[index], latest) : items;
    },
  };
}
