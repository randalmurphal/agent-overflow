import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { fireEvent, render, waitFor } from '@testing-library/svelte';
import { tick } from 'svelte';
import ActivityRailHost from '../../../test/mocks/ActivityRailTestHost.svelte';
import { buildPane } from '../../../test/helpers/chat';
import { resetBindingMocks, setBindingMock } from '../../../test/mocks/bindings-app';
import { emitWailsEvent } from '../../../test/mocks/wailsio-runtime';
import { getToasts, removeToast } from '../../stores/toast.svelte';
import { setupEventListeners } from '../../stores/events';
import { noteThread } from '../../transport/entityIndex';
import { __resetActivityRailUiPrefsForTest } from '../../stores/liveTodoState.svelte';
import { setCompactLayoutForTest } from '../../stores/layoutMode.svelte';
import type { ThreadWakeRow } from '../../stores/bindings';

function row(id: string, over: Partial<ThreadWakeRow> = {}): ThreadWakeRow {
  return { id, kind: 'loop', label: id, state: 'armed', nextAt: Date.now() + 12 * 60_000 + 30_000, ...over } as ThreadWakeRow;
}

const loop = row('loop-1', { label: 'check tests', lastOutcome: 'woke the thread', lastRunAt: Date.now() - 60_000 });
const watch = row('watch-1', { kind: 'watch', label: 'ci status', lastOutcome: 'error: exit 2' });
const reminder = row('token-1', { kind: 'reminder', label: 'review the deploy' });

async function openTray(rows: ThreadWakeRow[], moreReminders = 0) {
  noteThread('thread-1', '', 2);
  const pane = await buildPane();
  setBindingMock('ListThreadWakes', async () => ({ rows, moreReminders }));
  const view = render(ActivityRailHost, { props: { pane } });
  const toggle = await view.findByTestId('activity-rail-background-toggle');
  return { pane, view, toggle };
}

describe('wake rows in the background tray', () => {
  let stopEvents = () => {};
  beforeEach(() => {
    resetBindingMocks();
    __resetActivityRailUiPrefsForTest();
    for (const toast of getToasts().slice()) removeToast(toast.id);
    setBindingMock('ListLiveBackgroundTasks', async () => []);
    stopEvents = setupEventListeners();
  });
  afterEach(() => {
    setCompactLayoutForTest(false);
    stopEvents();
    vi.restoreAllMocks();
  });

  it('shows the tray for a thread with only wakes and counts listed and unlisted ones', async () => {
    const { view, toggle } = await openTray([loop, watch, reminder], 4);
    expect((await view.findByTestId('activity-rail-background-count')).textContent?.trim()).toBe('7');
    await fireEvent.click(toggle);
    const rows = await view.findAllByTestId('wake-tray-row');
    expect(rows.map((r) => r.dataset.kind)).toEqual(['loop', 'watch', 'reminder']);
    expect(rows.map((r) => r.querySelector('[data-testid="wake-tray-row-kind"]')?.textContent)).toEqual(['loop', 'watch', 'remind']);
    expect(rows[0].querySelector('[data-testid="wake-tray-row-when"]')?.textContent).toBe('next 12m');
    expect(rows[0].querySelector('[data-testid="wake-tray-row-outcome"]')?.textContent).toBe('woke the thread');
    expect(rows[1].querySelector('[data-testid="wake-tray-row-outcome"]')?.getAttribute('data-tone')).toBe('error');
    expect(view.getByTestId('wake-tray-more-reminders').textContent).toContain('4 more reminders');
  });

  it('under compact, gives the label its own line and puts when and the actions on the next', async () => {
    setCompactLayoutForTest(true);
    const { view, toggle } = await openTray([reminder]);
    await fireEvent.click(toggle);
    const r = await view.findByTestId('wake-tray-row');
    const label = r.querySelector('[data-testid="wake-tray-row-label"]')!;
    const controls = r.querySelector('[data-testid="wake-tray-row-controls"]')!;
    expect(label.classList.contains('truncate')).toBe(false);
    expect(label.classList.contains('line-clamp-2')).toBe(true);
    expect(label.textContent).toBe('review the deploy');
    expect(label.closest('[data-testid="wake-tray-row-controls"]')).toBeNull();
    for (const id of ['wake-tray-row-when', 'wake-tray-row-wake-now', 'wake-tray-row-cancel']) {
      expect(controls.querySelector(`[data-testid="${id}"]`), id).not.toBeNull();
      expect(r.querySelectorAll(`[data-testid="${id}"]`).length, id).toBe(1);
    }
    expect(controls.querySelector('[data-testid="wake-tray-row-when"]')?.textContent).toBe('next 12m');

    // Desktop keeps the one line.
    setCompactLayoutForTest(false);
    await tick();
    expect(r.querySelector('[data-testid="wake-tray-row-controls"]')).toBeNull();
    expect(r.querySelector('[data-testid="wake-tray-row-label"]')?.classList.contains('truncate')).toBe(true);
    expect(r.querySelectorAll('[data-testid="wake-tray-row-cancel"]').length).toBe(1);
  });

  it('says running for a row in flight and refuses Wake now there with the reason', async () => {
    const { view, toggle } = await openTray([
      row('loop-1', { state: 'running' }),
      row('watch-1', { kind: 'watch', state: 'running', longRunning: true, nextAt: 0 }),
    ]);
    await fireEvent.click(toggle);
    const [running, standing] = await view.findAllByTestId('wake-tray-row');
    for (const r of [running, standing]) {
      expect(r.querySelector('[data-testid="wake-tray-row-when"]')?.textContent).toBe('running');
      expect((r.querySelector('[data-testid="wake-tray-row-wake-now"]') as HTMLButtonElement).disabled).toBe(true);
    }
    expect(running.querySelector('[data-testid="wake-tray-row-wake-now"]')?.getAttribute('title')).toBe('Already running');
    // The tray says both are running; only the run in flight pulses the toggle.
    expect(view.getByTestId('activity-rail-background-running-label').textContent?.trim()).toBe('2 running');
    expect(view.queryByTestId('activity-rail-background-pulse')).not.toBeNull();
    expect(standing.querySelector('[data-testid="wake-tray-row-wake-now"]')?.getAttribute('title')).toMatch(/runs all the time/);
  });

  it('does not pulse the toggle for a long-running watch alone, though its row and the tray say running', async () => {
    const { view, toggle } = await openTray([row('watch-1', { kind: 'watch', state: 'running', longRunning: true, nextAt: 0 })]);
    expect(view.queryByTestId('activity-rail-background-pulse')).toBeNull();
    await fireEvent.click(toggle);
    expect((await view.findByTestId('activity-rail-background-running-label')).textContent?.trim()).toBe('1 running');
  });

  it('shows a wake that did not reach the thread as a warning with its missed fires', async () => {
    const { view, toggle } = await openTray([row('loop-1', { lastOutcome: 'woke the thread', undelivered: 'not delivered: the session ended', missed: 3 })]);
    await fireEvent.click(toggle);
    const outcome = await view.findByTestId('wake-tray-row-outcome');
    expect(outcome.textContent).toBe('not delivered: the session ended · 3 missed');
    expect(outcome.dataset.tone).toBe('warning');
  });

  it('fires and cancels each kind by its own id, and the row changes only when the backend says so', async () => {
    const wake = setBindingMock('WakeThreadWakeNow', async () => {});
    const cancel = setBindingMock('CancelThreadWake', async () => {});
    const { pane, view, toggle } = await openTray([loop, watch, reminder]);
    await fireEvent.click(toggle);
    const rows = await view.findAllByTestId('wake-tray-row');
    for (const r of rows) await fireEvent.click(r.querySelector('[data-testid="wake-tray-row-wake-now"]')!);
    for (const r of rows) await fireEvent.click(r.querySelector('[data-testid="wake-tray-row-cancel"]')!);
    await waitFor(() => expect(cancel).toHaveBeenCalledTimes(3));
    expect(wake.mock.calls).toEqual([[pane.threadId, 'loop', 'loop-1'], [pane.threadId, 'watch', 'watch-1'], [pane.threadId, 'reminder', 'token-1']]);
    expect(cancel.mock.calls).toEqual([[pane.threadId, 'loop', 'loop-1'], [pane.threadId, 'watch', 'watch-1'], [pane.threadId, 'reminder', 'token-1']]);
    expect(view.getAllByTestId('wake-tray-row')).toHaveLength(3);

    setBindingMock('ListThreadWakes', async () => ({ rows: [watch], moreReminders: 0 }));
    emitWailsEvent('thread:wakes_changed', { threadId: pane.threadId, id: 'wake-1' });
    await waitFor(() => expect(view.getAllByTestId('wake-tray-row').map((r) => r.dataset.kind)).toEqual(['watch']));
    expect((await view.findByTestId('activity-rail-background-count')).textContent?.trim()).toBe('1');
  });

  it('toasts the backend reason when Wake now or Cancel is refused', async () => {
    setBindingMock('WakeThreadWakeNow', async () => { throw new Error('the loop "check tests" was cancelled'); });
    setBindingMock('CancelThreadWake', async () => { throw new Error('read the reminder: database is locked'); });
    const { view, toggle } = await openTray([loop]);
    await fireEvent.click(toggle);
    await fireEvent.click(await view.findByTestId('wake-tray-row-wake-now'));
    await waitFor(() => expect(getToasts().map((t) => t.message)).toContain('Could not wake check tests: The loop "check tests" was cancelled.'));
    await fireEvent.click(view.getByTestId('wake-tray-row-cancel'));
    await waitFor(() => expect(getToasts().some((t) => t.message.startsWith('Could not cancel check tests:'))).toBe(true));
  });

  it('expands only a watch, reads its detail on open and again when it runs', async () => {
    const detail = setBindingMock('GetThreadWakeDetail', async () => ({ command: 'gh pr checks', cwd: '/work/repo', stderr: 'rate limited\n' }));
    const { pane, view, toggle } = await openTray([loop, watch]);
    await fireEvent.click(toggle);
    const [loopRow, watchRow] = await view.findAllByTestId('wake-tray-row');
    expect(loopRow.querySelector('[data-testid="wake-tray-row-toggle"]')?.getAttribute('aria-disabled')).toBe('true');
    await fireEvent.click(loopRow.querySelector('[data-testid="wake-tray-row-toggle"]')!);
    await tick();
    expect(view.queryByTestId('wake-tray-row-detail')).toBeNull();
    expect(detail).not.toHaveBeenCalled();

    await fireEvent.click(watchRow.querySelector('[data-testid="wake-tray-row-toggle"]')!);
    expect((await view.findByTestId('wake-tray-row-command')).textContent).toBe('gh pr checks');
    expect(view.getByTestId('wake-tray-row-cwd').textContent).toBe('/work/repo');
    expect(view.getByTestId('wake-tray-row-stderr').textContent).toBe('rate limited\n');
    expect(detail).toHaveBeenCalledWith(pane.threadId, 'watch-1');

    // A change to another row, or a read that changes nothing, leaves it be.
    setBindingMock('ListThreadWakes', async () => ({ rows: [{ ...loop, lastOutcome: 'skipped' }, { ...watch }], moreReminders: 0 }));
    emitWailsEvent('thread:wakes_changed', { threadId: pane.threadId, id: 'wake-1' });
    await waitFor(() => expect(view.getAllByTestId('wake-tray-row-outcome')[0].textContent).toBe('skipped'));
    expect(detail).toHaveBeenCalledTimes(1);

    detail.mockImplementation(async () => ({ command: 'gh pr checks', cwd: '/work/repo', stderr: '' }));
    setBindingMock('ListThreadWakes', async () => ({ rows: [loop, { ...watch, lastRunAt: Date.now(), lastOutcome: 'no change' }], moreReminders: 0 }));
    emitWailsEvent('thread:wakes_changed', { threadId: pane.threadId, id: 'wake-1' });
    expect(await view.findByTestId('wake-tray-row-stderr-empty')).toBeInTheDocument();
    expect(detail).toHaveBeenCalledTimes(2);

    await fireEvent.click(view.getAllByTestId('wake-tray-row')[1].querySelector('[data-testid="wake-tray-row-toggle"]')!);
    await tick();
    expect(view.queryByTestId('wake-tray-row-detail')).toBeNull();
  });
});
