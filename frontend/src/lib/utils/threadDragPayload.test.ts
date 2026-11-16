import { afterEach, describe, expect, it } from 'vitest';
import {
  beginThreadRowDrag,
  canDropThreadInGroup,
  canUngroupDroppedThread,
  encodeThreadDragPayload,
  beginToolPaneDrag,
  decodeToolDragPayload,
  encodeToolDragPayload,
  endThreadRowDrag,
  endToolPaneDrag,
  threadDragPayloadForEvent,
  toolDragPayloadForEvent,
  THREAD_ROW_DRAG_MIME,
  TOOL_PANE_DRAG_MIME,
} from './threadDragPayload';

describe('thread drag data', () => {
  it('carries the thread id and nothing else', () => {
    expect(encodeThreadDragPayload({ threadId: 'thread-1', projectId: 'project-1', groupId: 'group-1' })).toBe('thread-1');
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

  it('answers a drop with the record of the thread the DataTransfer names', () => {
    beginThreadRowDrag({ threadId: 'dragging', projectId: 'p1', groupId: 'g1' });
    expect(threadDragPayloadForEvent(evt([THREAD_ROW_DRAG_MIME], 'dragging')))
      .toEqual({ threadId: 'dragging', projectId: 'p1', groupId: 'g1' });
  });

  // A record left by a drag that ended unheard, or a drag from another
  // window of the app: the DataTransfer names the thread, and no project
  // is known for it, so only the pane strip can place it.
  it('answers a drop of another thread with its id alone', () => {
    beginThreadRowDrag({ threadId: 'stale', projectId: 'p9' });
    expect(threadDragPayloadForEvent(evt([THREAD_ROW_DRAG_MIME], 'live'))).toEqual({ threadId: 'live', projectId: '' });
    endThreadRowDrag();
    expect(threadDragPayloadForEvent(evt([THREAD_ROW_DRAG_MIME], 'live'))).toEqual({ threadId: 'live', projectId: '' });
  });

  it('falls back to the in-flight record when it does not', () => {
    beginThreadRowDrag({ threadId: 'dragging', projectId: 'p1', groupId: 'g1' });
    const payload = threadDragPayloadForEvent(evt([THREAD_ROW_DRAG_MIME], ''));
    expect(payload).toEqual({ threadId: 'dragging', projectId: 'p1', groupId: 'g1' });
  });

  it('never answers for a drag that is not a thread row', () => {
    beginThreadRowDrag({ threadId: 'dragging', projectId: 'p1' });
    expect(threadDragPayloadForEvent(evt(['Files'], ''))).toBeNull();
  });

  it('forgets the record once the drag ends', () => {
    beginThreadRowDrag({ threadId: 'dragging', projectId: 'p1' });
    endThreadRowDrag();
    expect(threadDragPayloadForEvent(evt([THREAD_ROW_DRAG_MIME], ''))).toBeNull();
  });
});

describe('group drop predicates', () => {
  const inG1 = { threadId: 't', projectId: 'p1', groupId: 'g1' };
  const loose = { threadId: 't', projectId: 'p1' };

  it('accepts a loose thread of the same project', () => {
    expect(canDropThreadInGroup(loose, 'p1', 'g1')).toBe(true);
  });

  it('refuses another project, and refuses the group the thread is already in', () => {
    expect(canDropThreadInGroup(loose, 'p2', 'g1')).toBe(false);
    expect(canDropThreadInGroup(inG1, 'p1', 'g1')).toBe(false);
    // An agent, or a thread an agent spawned, never joins a group.
    expect(canDropThreadInGroup({ ...loose, agentBound: true }, 'p1', 'g1')).toBe(false);
    expect(canDropThreadInGroup(inG1, 'p1', 'g2')).toBe(true);
  });

  it('ungroups only a grouped thread, and only in its own project', () => {
    expect(canUngroupDroppedThread(inG1, 'p1')).toBe(true);
    expect(canUngroupDroppedThread(inG1, 'p2')).toBe(false);
    expect(canUngroupDroppedThread(loose, 'p1')).toBe(false);
  });
});

// A tool button drags its kind under its own type, so a thread target never
// reads it as a thread and a pane drop never reads a thread as a tool.
describe('tool drag payloads', () => {
  afterEach(() => {
    endToolPaneDrag();
    endThreadRowDrag();
  });

  function evt(types: string[], data: Record<string, string>): DragEvent {
    return {
      dataTransfer: { types, getData: (type: string) => data[type] ?? '' },
    } as unknown as DragEvent;
  }

  it('round-trips a tool kind and refuses anything else', () => {
    expect(decodeToolDragPayload(encodeToolDragPayload({ kind: 'workflows' }))).toEqual({ kind: 'workflows' });
    expect(decodeToolDragPayload(encodeToolDragPayload({ kind: 'pulls' }))).toEqual({ kind: 'pulls' });
    expect(decodeToolDragPayload('{"kind":"thread"}')).toBeNull();
    expect(decodeToolDragPayload('{}')).toBeNull();
    expect(decodeToolDragPayload('null')).toBeNull();
    expect(decodeToolDragPayload('not json')).toBeNull();
  });

  it('reads the DataTransfer when it answers and the in-flight record when it does not', () => {
    beginToolPaneDrag({ kind: 'pulls' });
    expect(toolDragPayloadForEvent(evt([TOOL_PANE_DRAG_MIME], { [TOOL_PANE_DRAG_MIME]: encodeToolDragPayload({ kind: 'workflows' }) }))).toEqual({ kind: 'workflows' });
    expect(toolDragPayloadForEvent(evt([TOOL_PANE_DRAG_MIME], {}))).toEqual({ kind: 'pulls' });
    endToolPaneDrag();
    expect(toolDragPayloadForEvent(evt([TOOL_PANE_DRAG_MIME], {}))).toBeNull();
  });

  it('is never a thread, and a thread is never a tool', () => {
    beginToolPaneDrag({ kind: 'pulls' });
    beginThreadRowDrag({ threadId: 'stale', projectId: 'p1' });
    const tool = evt([TOOL_PANE_DRAG_MIME], { [TOOL_PANE_DRAG_MIME]: encodeToolDragPayload({ kind: 'pulls' }) });
    expect(threadDragPayloadForEvent(tool)).toBeNull();
    const thread = evt([THREAD_ROW_DRAG_MIME], { [THREAD_ROW_DRAG_MIME]: encodeThreadDragPayload({ threadId: 't', projectId: 'p1' }) });
    expect(toolDragPayloadForEvent(thread)).toBeNull();
    expect(toolDragPayloadForEvent(evt(['Files'], {}))).toBeNull();
    expect(TOOL_PANE_DRAG_MIME).not.toBe(THREAD_ROW_DRAG_MIME);
  });
});
