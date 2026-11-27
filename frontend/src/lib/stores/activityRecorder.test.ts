// The screen's half of the activity recorder: which input reports, how
// often, on which thread and layout, and what a failed report does. The
// computer's half (spans, windows, writes) is internal/activity.
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { setBindingMock } from '../../test/mocks/bindings-app';
import { methodNotFound } from '../../test/helpers/olderComputer';
import { TransportError } from '../transport/wsClient';
import { REPORT_EVERY_MS, installActivityRecorder, noteActivityFocus } from './activityRecorder';

const layout = vi.hoisted(() => ({ compact: false, screen: 'list' as 'list' | 'thread' }));
const panes = vi.hoisted(() => ({ focused: null as string | null }));
const diagnostics = vi.hoisted(() => [] as string[][]);

vi.mock('./layoutMode.svelte', () => ({
  isCompactLayout: () => layout.compact,
  getCompactScreen: () => layout.screen,
}));
vi.mock('./panes.svelte', () => ({
  getFocusedPaneOrNull: () => (panes.focused ? { threadId: panes.focused } : null),
}));
vi.mock('../utils/frontendErrorCapture', () => ({
  reportFrontendDiagnostic: (message: string, detail = '') => { diagnostics.push([message, detail]); },
}));

let record: ReturnType<typeof setBindingMock>;
let dispose: () => void;

function input(type = 'keydown'): void {
  window.dispatchEvent(new Event(type));
}

async function settle(): Promise<void> {
  await vi.advanceTimersByTimeAsync(0);
}

describe('activity recorder', () => {
  beforeEach(() => {
    vi.useFakeTimers({ toFake: ['setTimeout', 'Date'] });
    layout.compact = false;
    layout.screen = 'list';
    panes.focused = 't-a';
    diagnostics.length = 0;
    record = setBindingMock('RecordActivity', async () => undefined);
    dispose = installActivityRecorder();
  });

  afterEach(() => {
    dispose();
    vi.useRealTimers();
  });

  it('reports the first input, then the first input a report interval later', async () => {
    input();
    await settle();
    expect(record.mock.calls).toEqual([['t-a', false]]);
    await vi.advanceTimersByTimeAsync(REPORT_EVERY_MS / 2);
    input('pointerdown');
    input('wheel');
    await settle();
    expect(record).toHaveBeenCalledTimes(1);
    await vi.advanceTimersByTimeAsync(REPORT_EVERY_MS / 2);
    input('touchstart');
    await settle();
    expect(record).toHaveBeenCalledTimes(2);
  });

  it('reports the thread a press moved to, after the press\'s own handlers', async () => {
    input('pointerdown');
    panes.focused = 't-b';
    await settle();
    expect(record.mock.calls).toEqual([['t-b', false]]);
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
    panes.focused = 't-b';
    noteActivityFocus();
    await settle();
    expect(record.mock.calls).toEqual([['t-a', false], ['t-b', false]]);
    await vi.advanceTimersByTimeAsync(REPORT_EVERY_MS);
    panes.focused = 't-c';
    noteActivityFocus();
    await settle();
    expect(record).toHaveBeenCalledTimes(2);
  });

  it('reports the compact layout as the phone, and its list as no thread', async () => {
    layout.compact = true;
    layout.screen = 'thread';
    input('touchstart');
    await settle();
    layout.screen = 'list';
    input('touchstart');
    noteActivityFocus();
    await settle();
    expect(record.mock.calls).toEqual([['t-a', true], ['', true]]);
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
