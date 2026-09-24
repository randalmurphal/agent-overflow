import { afterEach, describe, expect, it } from 'vitest';
import { fireEvent, render, waitFor } from '@testing-library/svelte';
import BackgroundKillConfirmationHost from './BackgroundKillConfirmationHost.svelte';
import {
  confirmBackgroundKill,
  pendingBackgroundKillConfirmation,
  resetForTest,
} from '../../stores/backgroundKillConfirmation.svelte';

const parked = { launchItemId: 'tu-a', description: 'gate watcher', runState: 'parked' as const, transcriptRootId: 'tu-a' };
const running = { launchItemId: 'tu-b', description: 'shard reviewer', runState: 'running' as const, transcriptRootId: 'tu-b' };

describe('<BackgroundKillConfirmationHost>', () => {
  afterEach(() => resetForTest());

  it('renders nothing until a Stop asks', () => {
    const { queryByTestId } = render(BackgroundKillConfirmationHost);
    expect(queryByTestId('background-kill-dialog')).toBeNull();
  });

  it('lists the agents the refused Stop named and answers "stop everything"', async () => {
    const { getByTestId, getAllByTestId } = render(BackgroundKillConfirmationHost);
    const answer = confirmBackgroundKill('t1', [parked, running]);
    await waitFor(() => expect(getByTestId('background-kill-dialog')).toBeInTheDocument());
    expect(getByTestId('background-kill-dialog').textContent).toContain('stops the 2 background agents');
    const rows = getAllByTestId('background-kill-agent');
    expect(rows).toHaveLength(2);
    expect(rows[0]).toHaveAttribute('data-run-state', 'parked');
    expect(rows[0].textContent).toContain('gate watcher');
    expect(rows[0].textContent).toContain('waiting on background commands');
    expect(rows[0].querySelector('[data-testid="indicator"]')?.getAttribute('data-state')).toBe('parked');
    expect(rows[1].textContent).toContain('shard reviewer');
    expect(rows[1].textContent).toContain('running');
    expect(rows[1].querySelector('[data-testid="indicator"]')?.getAttribute('data-state')).toBe('backgrounded');
    expect(getByTestId('background-kill-confirm').textContent).toContain('Stop everything');

    await fireEvent.click(getByTestId('background-kill-confirm'));
    await expect(answer).resolves.toBe(true);
    expect(pendingBackgroundKillConfirmation()).toBeNull();
  });

  it('words a single agent in the singular and answers "keep it" on cancel', async () => {
    const { getByTestId, queryByTestId } = render(BackgroundKillConfirmationHost);
    const answer = confirmBackgroundKill('t1', [parked]);
    await waitFor(() => expect(getByTestId('background-kill-dialog')).toBeInTheDocument());
    expect(getByTestId('background-kill-dialog').textContent).toContain('stops the background agent still working. It will not');
    expect(getByTestId('background-kill-confirm').textContent).toContain('Stop both');
    expect(getByTestId('background-kill-cancel')).toHaveFocus();
    await fireEvent.click(getByTestId('background-kill-cancel'));
    await expect(answer).resolves.toBe(false);
    await waitFor(() => expect(queryByTestId('background-kill-dialog')).toBeNull());
  });

  it('still asks, without a list, when the refusal named no readable agents; Escape keeps them', async () => {
    const { getByTestId, queryByTestId } = render(BackgroundKillConfirmationHost);
    const answer = confirmBackgroundKill('t1', []);
    await waitFor(() => expect(getByTestId('background-kill-dialog')).toBeInTheDocument());
    expect(getByTestId('background-kill-dialog').textContent).toContain('stops the background agents still working');
    expect(queryByTestId('background-kill-agents')).toBeNull();
    await fireEvent.keyDown(document.activeElement ?? document.body, { key: 'Escape' });
    await expect(answer).resolves.toBe(false);
  });
});
