import { afterEach, describe, expect, it } from 'vitest';
import {
  countMountedTimelineMemoryNodes,
  installTimelineMemoryDiagnostics,
  type TimelineMemoryStats,
} from './timelineMemoryDiagnostics';

function makeStats(threadId: string): TimelineMemoryStats {
  return {
    threadId,
    itemWindowItems: 1,
    revealedNodes: 1,
    oldestLoadedCursor: { turnIndex: 0, itemIndex: 0, itemId: 'a' },
    newestLoadedCursor: { turnIndex: 0, itemIndex: 0, itemId: 'a' },
    oldestLoadedTurnIndex: 0,
    newestLoadedTurnIndex: 0,
    hasMoreHistory: false,
    hasMoreNewer: false,
    loadingOlder: false,
    loadingNewer: false,
    mountedTimelineNodes: 0,
    mountedDiffBlocks: 0,
    mountedDiffBodies: 0,
    mountedDiffSidebarFiles: 0,
    mountedDiffSidebarBodies: 0,
    paneState: {},
  };
}

describe('timelineMemoryDiagnostics', () => {
  afterEach(() => {
    delete window.__agentOverflowTimelineMemoryStats;
    delete window.__agentOverflowTimelineMemoryStatsByPane;
    document.body.innerHTML = '';
  });

  it('installs and removes the active timeline stats getter', () => {
    const disposeFirst = installTimelineMemoryDiagnostics('pane-1', () => makeStats('first'));
    expect(window.__agentOverflowTimelineMemoryStats?.().threadId).toBe('first');

    const disposeSecond = installTimelineMemoryDiagnostics('pane-2', () => makeStats('second'));
    expect(window.__agentOverflowTimelineMemoryStats?.().threadId).toBe('second');
    expect(Object.keys(window.__agentOverflowTimelineMemoryStatsByPane?.() ?? {})).toEqual([
      'pane-1',
      'pane-2',
    ]);

    disposeFirst();
    expect(window.__agentOverflowTimelineMemoryStats?.().threadId).toBe('second');

    disposeSecond();
    expect(window.__agentOverflowTimelineMemoryStats).toBeUndefined();
    expect(window.__agentOverflowTimelineMemoryStatsByPane).toBeUndefined();
  });

  it('counts mounted timeline and diff nodes under the supplied root', () => {
    document.body.innerHTML = `
      <section id="target">
        <div data-testid="message-timeline-node"></div>
        <div data-testid="diff-file-block"></div>
        <div data-testid="diff-file-body"></div>
        <div data-testid="diff-sidebar-file"></div>
        <div data-testid="diff-sidebar-file-body"></div>
      </section>
      <section id="other">
        <div data-testid="message-timeline-node"></div>
        <div data-testid="diff-file-block"></div>
      </section>
    `;

    expect(countMountedTimelineMemoryNodes(document.getElementById('target'))).toEqual({
      mountedTimelineNodes: 1,
      mountedDiffBlocks: 1,
      mountedDiffBodies: 1,
      mountedDiffSidebarFiles: 1,
      mountedDiffSidebarBodies: 1,
    });
  });
});
