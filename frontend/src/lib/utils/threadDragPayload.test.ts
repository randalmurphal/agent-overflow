import { afterEach, describe, expect, it } from 'vitest';
import {
  beginThreadRowDrag,
  canDropThreadInGroup,
  canUngroupDroppedThread,
  decodeThreadDragPayload,
  encodeThreadDragPayload,
  endThreadRowDrag,
  projectedPaneDropWidth,
  threadDragPayloadForEvent,
  THREAD_ROW_DRAG_MIME,
} from './threadDragPayload';

describe('pane thread drop helpers', () => {
  it('round-trips thread drag payloads', () => {
    const raw = encodeThreadDragPayload({
      threadId: 'thread-1',
      title: 'Build UI',
      projectId: 'project-1',
    });

    expect(decodeThreadDragPayload(raw)).toEqual({
      threadId: 'thread-1',
      title: 'Build UI',
      projectId: 'project-1',
    });
  });

  it('round-trips the source group so an ungroup drop knows what it left', () => {
    const raw = encodeThreadDragPayload({
      threadId: 'thread-1',
      title: 'Build UI',
      projectId: 'project-1',
      groupId: 'group-1',
    });

    expect(decodeThreadDragPayload(raw)).toEqual({
      threadId: 'thread-1',
      title: 'Build UI',
      projectId: 'project-1',
      groupId: 'group-1',
    });
  });

  it('omits groupId entirely for an ungrouped thread', () => {
    const decoded = decodeThreadDragPayload(
      encodeThreadDragPayload({ threadId: 't', title: 'x', projectId: 'p' }),
    );
    expect(decoded).not.toBeNull();
    expect('groupId' in (decoded as object)).toBe(false);
  });

  it('decodes a payload with no project or group into a usable pane drop', () => {
    // A pane drop needs neither field, and a payload minted before they
    // existed must not decode to null and swallow the drag.
    expect(decodeThreadDragPayload(JSON.stringify({ threadId: 't', title: 'x' }))).toEqual({
      threadId: 't',
      title: 'x',
      projectId: '',
    });
  });

  it('drops a non-string projectId / groupId rather than trusting it', () => {
    expect(
      decodeThreadDragPayload(JSON.stringify({ threadId: 't', title: 'x', projectId: 7, groupId: 9 })),
    ).toEqual({ threadId: 't', title: 'x', projectId: '' });
    expect(
      decodeThreadDragPayload(JSON.stringify({ threadId: 't', title: 'x', projectId: 'p', groupId: '' })),
    ).toEqual({ threadId: 't', title: 'x', projectId: 'p' });
  });

  it('returns null for malformed JSON', () => {
    expect(decodeThreadDragPayload('{not json')).toBeNull();
    expect(decodeThreadDragPayload('')).toBeNull();
  });

  it('returns null when threadId is missing', () => {
    expect(decodeThreadDragPayload(JSON.stringify({ title: 'Just a title' }))).toBeNull();
  });

  it('returns null when threadId is not a string', () => {
    expect(decodeThreadDragPayload(JSON.stringify({ threadId: 42, title: 'x' }))).toBeNull();
    expect(decodeThreadDragPayload(JSON.stringify({ threadId: null, title: 'x' }))).toBeNull();
  });

  it("defaults title to 'Untitled' when missing or non-string", () => {
    expect(decodeThreadDragPayload(JSON.stringify({ threadId: 't' }))).toEqual({
      threadId: 't',
      title: 'Untitled',
      projectId: '',
    });
    expect(decodeThreadDragPayload(JSON.stringify({ threadId: 't', title: 7 }))).toEqual({
      threadId: 't',
      title: 'Untitled',
      projectId: '',
    });
  });

  it('projects the fit-mode share of the row for the new pane', () => {
    // avg 800 of a 2400 post-insert total over a 3200px row -> 1067.
    const width = projectedPaneDropWidth([
      { widthPx: 700 },
      { widthPx: 900 },
    ], 3200, 560);

    expect(width).toBe(1067);
  });

  it('meets exactly at the average where fit and overflow regimes cross', () => {
    // The two regimes cross where paneRowWidth === total + average. Here
    // total 1800, average 900, row 2700: the fit share equals the base
    // width, so both formulas agree at 900.
    const width = projectedPaneDropWidth(
      [{ widthPx: 900 }, { widthPx: 900 }],
      2700,
      560,
    );

    expect(width).toBe(900);
  });

  it('projects the average base width when the strip overflows', () => {
    // Post-insert total (4000) exceeds the row (1000): no stretch, the
    // new pane lands at its base width (the average) verbatim.
    const width = projectedPaneDropWidth(
      Array.from({ length: 4 }, () => ({ widthPx: 800 })),
      1000,
      560,
    );

    expect(width).toBe(800);
  });

  it('uses full available width for the first dropped pane', () => {
    expect(projectedPaneDropWidth([], 900, 560)).toBe(900);
  });

  it('clamps the projection to minPaneWidth when the share is small', () => {
    // four 100px widths -> share = 100/500 -> 200 of 1000; average is
    // even smaller. minPaneWidth (560) wins.
    const width = projectedPaneDropWidth(
      Array.from({ length: 4 }, () => ({ widthPx: 100 })),
      1000,
      560,
    );
    expect(width).toBe(560);
  });

  it('falls back to minPaneWidth when widths sum to zero', () => {
    expect(projectedPaneDropWidth([{ widthPx: 0 }, { widthPx: 0 }], 1000, 320)).toBe(320);
  });

  it('falls back to minPaneWidth when widths are non-finite', () => {
    expect(projectedPaneDropWidth([{ widthPx: Number.NaN }], 1000, 240)).toBe(240);
    expect(projectedPaneDropWidth([{ widthPx: Number.POSITIVE_INFINITY }], 1000, 240)).toBe(240);
  });
});

// ── The in-flight record ─────────────────────────────────────────────────
//
// `DataTransfer.getData` answers nothing during `dragover` (the store is in
// protected mode until the drop), and the group targets have to decide there.
// The source records the payload in-process; the resolver prefers the
// DataTransfer and falls back to the record.

describe('threadDragPayloadForEvent', () => {
  afterEach(() => endThreadRowDrag());

  function evt(types: string[], raw: string): DragEvent {
    return {
      dataTransfer: {
        types,
        getData: (type: string) => (type === THREAD_ROW_DRAG_MIME ? raw : ''),
      },
    } as unknown as DragEvent;
  }

  it('reads the DataTransfer when it answers', () => {
    beginThreadRowDrag({ threadId: 'stale', title: 'Stale', projectId: 'p9' });
    const payload = threadDragPayloadForEvent(evt(
      [THREAD_ROW_DRAG_MIME],
      encodeThreadDragPayload({ threadId: 'live', title: 'Live', projectId: 'p1' }),
    ));
    expect(payload?.threadId).toBe('live');
  });

  it('falls back to the in-flight record when it does not', () => {
    beginThreadRowDrag({ threadId: 'dragging', title: 'Dragging', projectId: 'p1', groupId: 'g1' });
    const payload = threadDragPayloadForEvent(evt([THREAD_ROW_DRAG_MIME], ''));
    expect(payload).toEqual({ threadId: 'dragging', title: 'Dragging', projectId: 'p1', groupId: 'g1' });
  });

  it('never answers for a drag that is not a thread row', () => {
    beginThreadRowDrag({ threadId: 'dragging', title: 'Dragging', projectId: 'p1' });
    expect(threadDragPayloadForEvent(evt(['Files'], ''))).toBeNull();
  });

  it('forgets the record once the drag ends', () => {
    beginThreadRowDrag({ threadId: 'dragging', title: 'Dragging', projectId: 'p1' });
    endThreadRowDrag();
    expect(threadDragPayloadForEvent(evt([THREAD_ROW_DRAG_MIME], ''))).toBeNull();
  });
});

describe('group drop predicates', () => {
  const inG1 = { threadId: 't', title: 't', projectId: 'p1', groupId: 'g1' };
  const loose = { threadId: 't', title: 't', projectId: 'p1' };

  it('accepts a loose thread of the same project', () => {
    expect(canDropThreadInGroup(loose, 'p1', 'g1')).toBe(true);
  });

  it('refuses another project, and refuses the group the thread is already in', () => {
    expect(canDropThreadInGroup(loose, 'p2', 'g1')).toBe(false);
    expect(canDropThreadInGroup(inG1, 'p1', 'g1')).toBe(false);
    expect(canDropThreadInGroup(inG1, 'p1', 'g2')).toBe(true);
  });

  it('ungroups only a grouped thread, and only in its own project', () => {
    expect(canUngroupDroppedThread(inG1, 'p1')).toBe(true);
    expect(canUngroupDroppedThread(inG1, 'p2')).toBe(false);
    expect(canUngroupDroppedThread(loose, 'p1')).toBe(false);
  });
});
