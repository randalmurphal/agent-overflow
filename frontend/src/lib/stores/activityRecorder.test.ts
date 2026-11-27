// The screen's half of the activity recorder: which input reports, how
// often, on which thread and layout, and what a failed report does. The
// computer's half (spans, windows, writes) is internal/activity.
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { setBindingMock } from '../../test/mocks/bindings-app';
import { methodNotFound } from '../../test/helpers/olderComputer';
import { TransportError } from '../transport/wsClient';
import { REPORT_EVERY_MS, installActivityRecorder, noteActivityFocus } from './activityRecorder';

const layout = vi.hoisted(() => ({ compact: false, screen: 'list' as 'list' | 'thread' }));
// focused is the focused pane's thread; target the pane the screen shows
// (paneCommandTarget), and threads each pane's thread where it is another
// pane's: a companion's.
const panes = vi.hoisted(() => ({ focused: null as string | null, target: null as string | null, threads: {} as Record<string, string> }));
const diagnostics = vi.hoisted(() => [] as string[][]);

vi.mock('./layoutMode.svelte', () => ({
  isCompactLayout: () => layout.compact,
  getCompactScreen: () => layout.screen,
}));
vi.mock('./panes.svelte', () => ({
  getFocusedPaneOrNull: () => (panes.focused ? { threadId: panes.focused } : null),
  paneCommandTarget: () => panes.target,
  getThreadPaneOf: (id: string | null) => (id ? { threadId: panes.threads[id] ?? id } : null),
}));
vi.mock('../utils/frontendErrorCapture', () => ({
  reportFrontendDiagnostic: (message: string, detail = '') => { diagnostics.push([message, detail]); },
}));

let record: ReturnType<typeof setBindingMock>;
let dispose: () => void;

function input(type = 'keydown'): void {
  window.dispatchEvent(new Event(type));
}

/** The screen shows the pane of thread, which has focus. */
function show(thread: string | null): void {
  panes.focused = thread;
  panes.target = thread;
}

async function settle(): Promise<void> {
  await vi.advanceTimersByTimeAsync(0);
}

describe('activity recorder', () => {
  beforeEach(() => {
    vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout', 'Date'] });
    layout.compact = false;
    layout.screen = 'list';
    show('t-a');
    panes.threads = {};
    diagnostics.length = 0;
    record = setBindingMock('RecordActivity', async () => undefined);
    dispose = installActivityRecorder();
  });

  afterEach(() => {
    dispose();
    vi.useRealTimers();
  });

  it('reports the first input, then input within the interval once the interval is up, with how long ago it was', async () => {
    input();
    await settle();
    expect(record.mock.calls).toEqual([['t-a', false, 0]]);
    await vi.advanceTimersByTimeAsync(REPORT_EVERY_MS / 2);
    input('pointerdown');
    await vi.advanceTimersByTimeAsync(REPORT_EVERY_MS / 4);
    input('wheel');
    await settle();
    expect(record).toHaveBeenCalledTimes(1);
    await vi.advanceTimersByTimeAsync(REPORT_EVERY_MS / 4);
    expect(record.mock.calls).toEqual([['t-a', false, 0], ['t-a', false, REPORT_EVERY_MS / 4]]);
    input('touchstart');
    await settle();
    expect(record).toHaveBeenCalledTimes(2);
  });

  it('reports the last input of a burst, and nothing when there was none after a report', async () => {
    input();
    await settle();
    await vi.advanceTimersByTimeAsync(50_000);
    input();
    await vi.advanceTimersByTimeAsync(10 * REPORT_EVERY_MS);
    expect(record.mock.calls).toEqual([['t-a', false, 0], ['t-a', false, 10_000]]);
  });

  it('reports no input it holds once removed', async () => {
    input();
    await settle();
    await vi.advanceTimersByTimeAsync(REPORT_EVERY_MS / 2);
    input();
    dispose();
    await vi.advanceTimersByTimeAsync(REPORT_EVERY_MS);
    expect(record).toHaveBeenCalledTimes(1);
    dispose = installActivityRecorder();
  });

  it('reports the thread of the pane the compact screen shows, a companion opened from its source included', async () => {
    layout.compact = true;
    layout.screen = 'thread';
    input('touchstart');
    await settle();
    // An agent thread opened from the Agents rail shows in its companion
    // pane, while focus stays on the source thread.
    panes.target = 'p-agent';
    panes.threads = { 'p-agent': 't-agent' };
    input('touchstart');
    noteActivityFocus();
    await settle();
    expect(record.mock.calls).toEqual([['t-a', true, 0], ['t-agent', true, 0]]);
  });

  it('reports the thread a press moved to, after the press\'s own handlers', async () => {
    input('pointerdown');
    show('t-b');
    await settle();
    expect(record.mock.calls).toEqual([['t-b', false, 0]]);
  });

  it('counts no movement of the pointer or the mouse', async () => {
    input('pointermove');
    input('mousemove');
    await settle();
    expect(record).not.toHaveBeenCalled();
  });

  it('listens in the capture phase, passively', () => {
    dispose();
    const add = vi.spyOn(window, 'addEventListener');
    dispose = installActivityRecorder();
    expect(add.mock.calls.map(([name, , options]) => [name, options])).toEqual(
      ['keydown', 'pointerdown', 'wheel', 'touchstart'].map((name) => [name, { capture: true, passive: true }]),
    );
    add.mockRestore();
  });

  it('reports at once a thread the screen moved to after input, and not one it moved to unattended', async () => {
    input();
    await settle();
    input('pointerdown');
    show('t-b');
    noteActivityFocus();
    await settle();
    expect(record.mock.calls).toEqual([['t-a', false, 0], ['t-b', false, 0]]);
    await vi.advanceTimersByTimeAsync(REPORT_EVERY_MS);
    show('t-c');
    noteActivityFocus();
    await settle();
    expect(record).toHaveBeenCalledTimes(2);
  });

  it('reports a thread the screen moved to with no input of its own as of the move', async () => {
    input();
    await settle();
    await vi.advanceTimersByTimeAsync(50_000);
    // Another screen archives t-a: this one shows t-b, with no input here.
    show('t-b');
    noteActivityFocus();
    await settle();
    expect(record.mock.calls).toEqual([['t-a', false, 0], ['t-b', false, 0]]);
  });

  it('reports the compact layout as the phone, and its list as no thread', async () => {
    layout.compact = true;
    layout.screen = 'thread';
    input('touchstart');
    await settle();
    layout.screen = 'list';
    panes.target = null;
    input('touchstart');
    noteActivityFocus();
    await settle();
    expect(record.mock.calls).toEqual([['t-a', true, 0], ['', true, 0]]);
  });

  it('reports nothing once removed', async () => {
    dispose();
    input();
    await settle();
    expect(record).not.toHaveBeenCalled();
    dispose = installActivityRecorder();
  });

  it('records a failed report as a diagnostic, and nothing for a dropped connection or an older computer', async () => {
    record.mockRejectedValueOnce(new TransportError('forbidden', 'scope threads:read is required'));
    input();
    await settle();
    expect(diagnostics).toEqual([['An activity report failed', 'Scope threads:read is required.']]);
    for (const refusal of [methodNotFound(), new TransportError('auth_failed', 'session expired')]) {
      record.mockRejectedValueOnce(refusal);
      await vi.advanceTimersByTimeAsync(REPORT_EVERY_MS);
      input();
      await settle();
    }
    expect(record).toHaveBeenCalledTimes(3);
    expect(diagnostics).toHaveLength(1);
  });
});
