import { afterEach, describe, expect, it, vi } from 'vitest';
import {
  clearUiRenderTrace,
  flushUiRenderTrace,
  getUiRenderTraceRecords,
  installUiRenderTraceApi,
  isUiRenderTraceEnabled,
  recordUiTrace,
  scheduleDomUiTrace,
  setUiRenderTraceEnabled,
  snapshotChatDomForTrace,
} from './uiRenderTrace';
import { getBindingMock, setBindingMock } from '../../test/mocks/bindings-app';

describe('uiRenderTrace', () => {
  afterEach(() => {
    clearUiRenderTrace();
    setUiRenderTraceEnabled(false);
    delete window.__agentOverflowUiTrace;
    delete window.__stickState;
    delete window.__paneGeometryRecording;
    window.history.replaceState(null, '', '/');
    vi.restoreAllMocks();
    vi.useRealTimers();
  });

  it('does not record while disabled', () => {
    setUiRenderTraceEnabled(false);

    recordUiTrace('chat.state', { threadId: 't1' });

    expect(getUiRenderTraceRecords()).toEqual([]);
  });

  it('records bounded snapshots when enabled', () => {
    setUiRenderTraceEnabled(true);

    recordUiTrace('chat.state', { threadId: 't1' });

    const records = getUiRenderTraceRecords();
    expect(records).toHaveLength(1);
    expect(records[0]?.label).toBe('chat.state');
    expect(records[0]?.data).toEqual({ threadId: 't1' });
    expect(records[0]?.seq).toBeGreaterThan(0);
  });

  it('coalesces repeated DOM traces and throttles the next snapshot', async () => {
    vi.useFakeTimers();
    vi.setSystemTime(0);
    const frameCallbacks: FrameRequestCallback[] = [];
    vi.spyOn(globalThis, 'requestAnimationFrame').mockImplementation((callback) => {
      frameCallbacks.push(callback);
      return frameCallbacks.length;
    });
    vi.spyOn(globalThis, 'cancelAnimationFrame').mockImplementation(() => {});
    setUiRenderTraceEnabled(true);

    scheduleDomUiTrace('chat', 'chat.dom', () => ({ version: 'old' }));
    scheduleDomUiTrace('chat', 'chat.dom', () => ({ version: 'latest' }));

    expect(getUiRenderTraceRecords()).toEqual([]);
    expect(frameCallbacks).toHaveLength(1);
    frameCallbacks.shift()?.(0);

    expect(getUiRenderTraceRecords()).toMatchObject([
      { label: 'chat.dom', data: { version: 'latest' } },
    ]);

    scheduleDomUiTrace('chat', 'chat.dom', () => ({ version: 'delayed' }));
    expect(frameCallbacks).toHaveLength(0);
    await vi.advanceTimersByTimeAsync(249);
    expect(frameCallbacks).toHaveLength(0);

    await vi.advanceTimersByTimeAsync(1);
    expect(frameCallbacks).toHaveLength(1);
    frameCallbacks.shift()?.(250);

    expect(getUiRenderTraceRecords()).toMatchObject([
      { label: 'chat.dom', data: { version: 'latest' } },
      { label: 'chat.dom', data: { version: 'delayed' } },
    ]);
  });

  it('snapshots chat DOM row identity without reading row text previews', () => {
    const root = document.createElement('div');
    root.dataset.threadId = 'thread-1';
    root.innerHTML = `
      <div data-item-id="item-1">large rendered row text</div>
      <div data-item-id="item-2">another rendered row</div>
      <div data-testid="message-timeline-scroll"></div>
    `;

    const snapshot = snapshotChatDomForTrace(root);

    expect(snapshot.timelineRows).toEqual([
      { itemId: 'item-1' },
      { itemId: 'item-2' },
    ]);
  });

  it('flushes trace records to the backend in compact batches', async () => {
    setBindingMock('AppendUIRenderTraceBatch', async () => '/tmp/ui-render.jsonl');
    setUiRenderTraceEnabled(true);

    recordUiTrace('chat.state', { threadId: 't1' });

    const path = await flushUiRenderTrace();
    const appendTrace = getBindingMock('AppendUIRenderTraceBatch');
    const lines = appendTrace?.mock.calls[0]?.[0] as string[] | undefined;
    expect(path).toBe('/tmp/ui-render.jsonl');
    expect(lines).toHaveLength(1);
    expect(JSON.parse(lines?.[0] ?? '{}')).toMatchObject({
      label: 'chat.state',
      data: { threadId: 't1' },
    });
  });

  it('installs the dev console API', async () => {
    setBindingMock('AppendUIRenderTraceBatch', async () => '/tmp/ui-render.jsonl');
    installUiRenderTraceApi();

    window.__agentOverflowUiTrace?.enable();
    recordUiTrace('timeline.state', { itemCount: 2 });

    expect(isUiRenderTraceEnabled()).toBe(true);
    expect(window.__agentOverflowUiTrace?.recent(1)[0]?.label).toBe('timeline.state');
    expect(window.__agentOverflowUiTrace?.dump(1)).toContain('timeline.state');
    await expect(window.__agentOverflowUiTrace?.flush()).resolves.toBe('/tmp/ui-render.jsonl');

    window.__agentOverflowUiTrace?.clear();
    expect(window.__agentOverflowUiTrace?.records()).toEqual([]);

    window.__agentOverflowUiTrace?.disable();
    expect(isUiRenderTraceEnabled()).toBe(false);
  });

  it('Ctrl+Shift+B writes a user.bugReport marker, bookmarks the trace, and copies the bookmark path', async () => {
    setBindingMock('AppendUIRenderTraceBatch', async () => '/tmp/ui-render.jsonl');
    const bookmarkPath = '/tmp/ui-trace/bookmarks/bug-report-20260519T000000Z.jsonl';
    setBindingMock('BookmarkUIRenderTrace', async () => bookmarkPath);
    let clipboardText = '';
    Object.defineProperty(navigator, 'clipboard', {
      configurable: true,
      value: { writeText: vi.fn(async (text: string) => { clipboardText = text; }) },
    });
    installUiRenderTraceApi();
    window.__agentOverflowUiTrace?.enable();

    // Stub the stick-state hook so the marker carries something
    // distinctive.
    window.__stickState = () => ({ marker: 'sentinel' });
    window.history.pushState(null, '', '/trace?t=supersecret&access_token=access456&safe=1');

    // Synthesize Ctrl+Shift+B.
    const event = new KeyboardEvent('keydown', {
      ctrlKey: true,
      shiftKey: true,
      code: 'KeyB',
      bubbles: true,
      cancelable: true,
    });
    window.dispatchEvent(event);

    // The handler is async (flush + bookmark + clipboard); wait a tick.
    await new Promise<void>((resolve) => setTimeout(resolve, 0));

    const records = getUiRenderTraceRecords();
    const marker = records.find((r) => r.label === 'user.bugReport');
    expect(marker).toBeDefined();
    expect((marker?.data as { stickState: { marker: string } }).stickState.marker)
      .toBe('sentinel');
    expect(JSON.stringify(marker)).toContain('t=[redacted]&access_token=[redacted]&safe=1');
    expect(JSON.stringify(marker)).not.toContain('supersecret');
    expect(JSON.stringify(marker)).not.toContain('access456');
    expect(clipboardText).toBe(bookmarkPath);
  });

  it('Ctrl+Shift+B emits each armed recording frame as its own correlated line, not inline in the marker', async () => {
    // Regression guard: folding the whole ~80-frame rolling buffer into the
    // marker's `data` blew the 64KiB per-line trace cap, so the marker was
    // dropped as __droppedOversize and every capture came back empty. Frames must
    // go to separate `user.bugReportRecFrame` lines correlated by recId.
    setBindingMock('AppendUIRenderTraceBatch', async () => '/tmp/ui-render.jsonl');
    setBindingMock('BookmarkUIRenderTrace', async () => '/tmp/bookmark.jsonl');
    Object.defineProperty(navigator, 'clipboard', {
      configurable: true,
      value: { writeText: vi.fn(async () => {}) },
    });
    installUiRenderTraceApi();
    window.__agentOverflowUiTrace?.enable();

    // Arm: stub the rolling-buffer read hook with three frames.
    const frames = [
      { t: 0, panes: { main: { bottomRenderedIndex: 195 } } },
      { t: 100, panes: { main: { bottomRenderedIndex: 195 } } },
      { t: 200, panes: { main: { bottomRenderedIndex: 195 } } },
    ];
    window.__paneGeometryRecording = (() => frames) as never;

    window.dispatchEvent(new KeyboardEvent('keydown', {
      ctrlKey: true, shiftKey: true, code: 'KeyB', bubbles: true, cancelable: true,
    }));
    await new Promise<void>((resolve) => setTimeout(resolve, 0));

    const records = getUiRenderTraceRecords();
    const marker = records.find((r) => r.label === 'user.bugReport');
    expect(marker).toBeDefined();
    const recId = (marker?.data as { capturedAt: number }).capturedAt;
    // Marker carries only the count, not the payload.
    expect((marker?.data as { paneGeometryRecordingFrames: number }).paneGeometryRecordingFrames).toBe(3);
    expect(JSON.stringify(marker)).not.toContain('bottomRenderedIndex');

    // Three frame lines, each correlated to the marker and carrying its payload.
    const frameLines = records.filter((r) => r.label === 'user.bugReportRecFrame');
    expect(frameLines).toHaveLength(3);
    expect(frameLines.every((r) => (r.data as { recId: number }).recId === recId)).toBe(true);
    expect(frameLines.map((r) => (r.data as { t: number }).t)).toEqual([0, 100, 200]);
    expect((frameLines[0]?.data as { panes: { main?: unknown } }).panes.main).toBeDefined();

    delete window.__paneGeometryRecording;
  });

  it('Ctrl+Shift+B falls back to the live trace path when bookmarking fails', async () => {
    setBindingMock('AppendUIRenderTraceBatch', async () => '/tmp/ui-render.jsonl');
    setBindingMock('BookmarkUIRenderTrace', async () => {
      throw new Error('disk full');
    });
    let clipboardText = '';
    Object.defineProperty(navigator, 'clipboard', {
      configurable: true,
      value: { writeText: vi.fn(async (text: string) => { clipboardText = text; }) },
    });
    installUiRenderTraceApi();
    window.__agentOverflowUiTrace?.enable();

    const event = new KeyboardEvent('keydown', {
      ctrlKey: true,
      shiftKey: true,
      code: 'KeyB',
      bubbles: true,
      cancelable: true,
    });
    window.dispatchEvent(event);

    await new Promise<void>((resolve) => setTimeout(resolve, 0));

    // Falls back to the live trace path so the user can still locate
    // the marker even if the bookmark copy failed.
    expect(clipboardText).toBe('/tmp/ui-render.jsonl');
  });
});
