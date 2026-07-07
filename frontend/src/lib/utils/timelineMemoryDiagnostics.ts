const MEMORY_DIAGNOSTICS_BUILD_GATE =
  import.meta.env.DEV ||
  import.meta.env.MODE === 'test' ||
  import.meta.env.VITE_AGENT_OVERFLOW_UI_TRACE === '1';

export interface TimelineMemoryStats {
  threadId: string | null;
  itemWindowItems: number;
  revealedNodes: number;
  oldestLoadedCursor: { turnIndex: number; itemIndex: number; itemId?: string } | null;
  newestLoadedCursor: { turnIndex: number; itemIndex: number; itemId?: string } | null;
  oldestLoadedTurnIndex: number | null;
  newestLoadedTurnIndex: number | null;
  hasMoreHistory: boolean;
  hasMoreNewer: boolean;
  loadingOlder: boolean;
  loadingNewer: boolean;
  mountedTimelineNodes: number;
  mountedDiffBlocks: number;
  mountedDiffBodies: number;
  paneState: unknown;
}

declare global {
  interface Window {
    __agentOverflowTimelineMemoryStats?: () => TimelineMemoryStats;
    __agentOverflowTimelineMemoryStatsByPane?: () => Record<string, TimelineMemoryStats>;
  }
}

const timelineStatsGettersByPane = new Map<string, () => TimelineMemoryStats>();
let activeTimelineStatsPaneId: string | null = null;

export function installTimelineMemoryDiagnostics(
  paneId: string,
  getStats: () => TimelineMemoryStats,
): () => void {
  if (!MEMORY_DIAGNOSTICS_BUILD_GATE || typeof window === 'undefined') return () => {};
  timelineStatsGettersByPane.set(paneId, getStats);
  activeTimelineStatsPaneId = paneId;
  window.__agentOverflowTimelineMemoryStats = () => {
    const activeGetter =
      (activeTimelineStatsPaneId
        ? timelineStatsGettersByPane.get(activeTimelineStatsPaneId)
        : undefined) ??
      [...timelineStatsGettersByPane.values()].at(-1);
    return activeGetter?.() ?? getStats();
  };
  window.__agentOverflowTimelineMemoryStatsByPane = () => {
    const stats: Record<string, TimelineMemoryStats> = {};
    for (const [id, getter] of timelineStatsGettersByPane) {
      stats[id] = getter();
    }
    return stats;
  };
  return () => {
    if (timelineStatsGettersByPane.get(paneId) === getStats) {
      timelineStatsGettersByPane.delete(paneId);
    }
    if (activeTimelineStatsPaneId === paneId) {
      activeTimelineStatsPaneId = [...timelineStatsGettersByPane.keys()].at(-1) ?? null;
    }
    if (timelineStatsGettersByPane.size === 0) {
      delete window.__agentOverflowTimelineMemoryStats;
      delete window.__agentOverflowTimelineMemoryStatsByPane;
    }
  };
}

export function countMountedTimelineMemoryNodes(
  root?: ParentNode | null,
): Pick<
  TimelineMemoryStats,
  | 'mountedTimelineNodes'
  | 'mountedDiffBlocks'
  | 'mountedDiffBodies'
> {
  const queryRoot = root ?? (typeof document === 'undefined' ? null : document);
  if (!queryRoot) {
    return {
      mountedTimelineNodes: 0,
      mountedDiffBlocks: 0,
      mountedDiffBodies: 0,
    };
  }
  return {
    mountedTimelineNodes: queryRoot.querySelectorAll('[data-testid="message-timeline-node"]').length,
    mountedDiffBlocks: queryRoot.querySelectorAll('[data-testid="diff-file-block"]').length,
    mountedDiffBodies: queryRoot.querySelectorAll('[data-testid="diff-file-body"]').length,
  };
}
