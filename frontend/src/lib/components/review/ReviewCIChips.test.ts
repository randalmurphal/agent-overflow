import { fireEvent, render, screen, waitFor } from '@testing-library/svelte';
import { describe, expect, it, vi } from 'vitest';
import ReviewCIChips from './ReviewCIChips.svelte';
import type { CIPipeline } from '../../types/models';
import { setBindingMock } from '../../../test/mocks/bindings-app';

// A stage's job rows do one of three things: open the job's log, open its
// forge URL, or nothing. Only a row that opens a URL carries the link
// icon, and a row that does nothing is no button and keeps the list open.

const PIPELINE: CIPipeline = {
  status: 'success',
  stages: [{
    name: 'external',
    status: 'success',
    jobs: [
      { id: '20', name: 'unit', status: 'success', logsAvailable: true },
      { name: 'appveyor', status: 'success', logsAvailable: false, url: 'https://ci.example/b/1' },
      { name: 'CodeRabbit', status: 'success', logsAvailable: false },
    ],
  }],
};

function jobRow(name: string): HTMLElement {
  const row = screen.getAllByTestId('review-ci-job').find((el) => el.textContent?.includes(name));
  if (!row) throw new Error(`no job row ${name}`);
  return row;
}

async function openStage(): Promise<void> {
  await fireEvent.click(screen.getByTestId('review-ci-chip'));
  await waitFor(() => expect(screen.getAllByTestId('review-ci-job')).toHaveLength(3));
}

describe('<ReviewCIChips> job rows', () => {
  it('renders a log row and a link row as buttons, the link alone with its icon, and a row with neither as text', async () => {
    render(ReviewCIChips, { pipeline: PIPELINE, loading: false, error: null, onOpenJob: vi.fn() });
    await openStage();
    const log = jobRow('unit');
    const link = jobRow('appveyor');
    const none = jobRow('CodeRabbit');
    expect(log.tagName).toBe('BUTTON');
    expect(log.dataset.action).toBe('log');
    expect(log.querySelector('[data-testid="review-ci-job-link"]')).toBeNull();
    expect(link.tagName).toBe('BUTTON');
    expect(link.dataset.action).toBe('link');
    expect(link.querySelector('[data-testid="review-ci-job-link"]')).not.toBeNull();
    expect(none.tagName).toBe('DIV');
    expect(none.dataset.action).toBe('none');
    expect(none.querySelector('[data-testid="review-ci-job-link"]')).toBeNull();
    expect(none.className).not.toContain('hover:');
    expect(none.textContent).toContain('CodeRabbit');
    expect(none.textContent).toContain('success');
    expect(screen.queryByRole('button', { name: /CodeRabbit/ })).toBeNull();
  });

  it('opens the log, opens the link, and leaves the list open on a row that does nothing', async () => {
    const onOpenJob = vi.fn();
    const open = setBindingMock('OpenExternalURL', async () => undefined);
    render(ReviewCIChips, { pipeline: PIPELINE, loading: false, error: null, onOpenJob });

    await openStage();
    await fireEvent.click(jobRow('CodeRabbit'));
    expect(screen.getAllByTestId('review-ci-job')).toHaveLength(3);
    expect(onOpenJob).not.toHaveBeenCalled();
    expect(open).not.toHaveBeenCalled();

    await fireEvent.click(jobRow('appveyor'));
    expect(open).toHaveBeenCalledWith('https://ci.example/b/1');
    await waitFor(() => expect(screen.queryAllByTestId('review-ci-job')).toHaveLength(0));

    await openStage();
    await fireEvent.click(jobRow('unit'));
    expect(onOpenJob).toHaveBeenCalledWith('external', expect.objectContaining({ id: '20' }));
  });
});
