import { fireEvent, render, screen, waitFor, within } from '@testing-library/svelte';
import { describe, expect, it, vi } from 'vitest';
import { SvelteSet } from 'svelte/reactivity';
import ReviewCIChips from './ReviewCIChips.svelte';
import ReviewCILogView from './ReviewCILogView.svelte';
import {
  ciJobActionKinds,
  ciJobHasArtifacts,
  ciPipelineActionKinds,
  ciPipelineHasArtifacts,
  type CIActionKind,
  type CIPendingAction,
} from '../../stores/ciActions.svelte';
import type { CIActionsView } from '../../stores/ciSubjectActions.svelte';
import type { CIJob, CIPipeline } from '../../types/models';

// The CI actions as the chips, a job's row and the log view's header
// render them: only the actions the read's flags allow, a pipeline menu
// only when it has something to offer, hover-revealed row actions that
// stay visible on compact and while pending, the pressed control pending
// and the rest waiting with the reason, and the log view's note that it
// followed a retry.

function job(id: string, name: string, actions: CIJob['actions']): CIJob {
  return { id, name, status: 'failed', logsAvailable: true, actions };
}

function pipeline(actions: CIPipeline['actions'], jobs: CIJob[]): CIPipeline {
  return { id: '77', status: 'failed', stages: [{ name: 'test', status: 'failed', jobs }], actions };
}

function fakeActions(read: CIPipeline, pending: CIPendingAction | null = null, downloading = false) {
  const run = vi.fn(async (_kind: CIActionKind, _job: CIJob | null) => true);
  const saveArtifacts = vi.fn(async (_job: CIJob | null) => null);
  const actions: CIActionsView = {
    pending,
    pipelineActions: ciPipelineActionKinds(read),
    pipelineHasArtifacts: ciPipelineHasArtifacts(read),
    actionsUnavailable: read.actionsUnavailable || null,
    jobActions: ciJobActionKinds,
    jobHasArtifacts: ciJobHasArtifacts,
    isPending: (kind, at) => pending !== null && pending.kind === kind && pending.jobId === (at?.id ?? null),
    artifactsDownloading: () => downloading,
    run,
    saveArtifacts,
  };
  return { actions, run, saveArtifacts };
}

const JOBS = [
  job('20', 'unit', { retry: 'allowed', artifacts: 'allowed' }),
  job('21', 'lint', { retry: 'denied', artifacts: 'denied' }),
  job('22', 'e2e', { retry: '' }),
];

async function openStage(): Promise<void> {
  await fireEvent.click(screen.getByTestId('review-ci-chip'));
  await waitFor(() => expect(screen.getAllByTestId('review-ci-job')).toHaveLength(3));
}

function rowActions(name: string): HTMLElement {
  const row = screen.getAllByTestId('review-ci-job-actions').find((el) => el.dataset.job === name);
  if (!row) throw new Error(`no actions for ${name}`);
  return row;
}

describe('<ReviewCIChips> actions', () => {
  it('offers no menu and no row actions without actions, or with nothing allowed', async () => {
    const read = pipeline({ retryFailed: 'denied', cancel: '' }, JOBS);
    const { unmount } = render(ReviewCIChips, { pipeline: read, loading: false, error: null, onOpenJob: vi.fn() });
    await openStage();
    expect(screen.queryByTestId('review-ci-pipeline-actions')).toBeNull();
    expect(screen.queryAllByTestId('review-ci-job-actions')).toHaveLength(0);
    unmount();
    render(ReviewCIChips, { pipeline: read, loading: false, error: null, onOpenJob: vi.fn(), actions: fakeActions(read).actions });
    expect(screen.queryByTestId('review-ci-pipeline-actions')).toBeNull();
  });

  it('shows why actions are unavailable as a muted row, the trigger shown for it alone', async () => {
    const sentence = 'Retry, cancel and artifact downloads are not shown: this login cannot read the Actions permissions or artifacts of o/r on GitHub.';
    const read = { ...pipeline({ retryFailed: '' }, JOBS), actionsUnavailable: sentence };
    render(ReviewCIChips, { pipeline: read, loading: false, error: null, onOpenJob: vi.fn(), actions: fakeActions(read).actions });
    await fireEvent.click(screen.getByTestId('review-ci-pipeline-actions'));
    const row = await screen.findByTestId('review-ci-pipeline-unavailable');
    expect(row.textContent?.trim()).toBe(sentence);
    expect(row.getAttribute('role')).toBe('presentation');
    expect(within(screen.getByRole('menu')).queryAllByRole('menuitem')).toHaveLength(0);
  });

  it('runs a pipeline action from the CI header\'s menu', async () => {
    const read = pipeline({ retryFailed: 'allowed', cancel: 'denied', artifacts: 'allowed' }, JOBS);
    const { actions, run, saveArtifacts } = fakeActions(read);
    render(ReviewCIChips, { pipeline: read, loading: false, error: null, onOpenJob: vi.fn(), actions });
    await fireEvent.click(screen.getByTestId('review-ci-pipeline-actions'));
    expect(screen.getByTestId('review-ci-pipeline-retry_failed')).toBeTruthy();
    expect(screen.queryByTestId('review-ci-pipeline-cancel_pipeline')).toBeNull();
    await fireEvent.click(screen.getByTestId('review-ci-pipeline-retry_failed'));
    expect(run).toHaveBeenCalledWith('retry_failed', null);
    await fireEvent.click(screen.getByTestId('review-ci-pipeline-actions'));
    await fireEvent.click(screen.getByTestId('review-ci-pipeline-artifacts'));
    expect(saveArtifacts).toHaveBeenCalledWith(null);
  });

  it('shows a pending pipeline action on the trigger and holds the menu\'s rows', async () => {
    const read = pipeline({ retryFailed: 'allowed' }, JOBS);
    render(ReviewCIChips, { pipeline: read, loading: false, error: null, onOpenJob: vi.fn(), actions: fakeActions(read, { kind: 'retry_failed', jobId: null }).actions });
    const trigger = screen.getByTestId('review-ci-pipeline-actions');
    expect(trigger.dataset.pending).toBe('true');
    expect(trigger.getAttribute('title')).toBe('Retrying the failed jobs…');
    await fireEvent.click(trigger);
    expect(screen.getByTestId('review-ci-pipeline-retry_failed').getAttribute('aria-disabled')).toBe('true');
  });

  it('reveals a job\'s allowed actions on hover, always on compact', async () => {
    const read = pipeline({}, JOBS);
    const { actions, run, saveArtifacts } = fakeActions(read);
    render(ReviewCIChips, { pipeline: read, loading: false, error: null, onOpenJob: vi.fn(), actions });
    await openStage();
    const unit = rowActions('unit');
    expect(unit.className).toContain('opacity-0');
    expect(unit.className).toContain('group-hover:opacity-100');
    expect(unit.className).toContain('compact:opacity-100');
    await fireEvent.click(within(unit).getByTestId('review-ci-job-retry_job'));
    expect(run).toHaveBeenCalledWith('retry_job', JOBS[0]);
    await fireEvent.click(within(unit).getByTestId('review-ci-job-artifacts'));
    expect(saveArtifacts).toHaveBeenCalledWith(JOBS[0]);
    // Denied and not-applying flags show nothing.
    expect(within(rowActions('lint')).queryAllByRole('button')).toHaveLength(0);
    expect(within(rowActions('e2e')).queryAllByRole('button')).toHaveLength(0);
  });

  it('keeps a pending job action shown, and the rest waiting with the reason', async () => {
    const jobs = [JOBS[0]!, job('23', 'docs', { retry: 'allowed' })];
    const read = pipeline({}, jobs);
    render(ReviewCIChips, { pipeline: read, loading: false, error: null, onOpenJob: vi.fn(), actions: fakeActions(read, { kind: 'retry_job', jobId: '20' }).actions });
    await waitFor(() => fireEvent.click(screen.getByTestId('review-ci-chip')));
    const unit = rowActions('unit');
    expect(unit.className).toContain('opacity-100');
    expect(unit.className).not.toMatch(/(^|\s)opacity-0/);
    const pressed = within(unit).getByTestId('review-ci-job-retry_job') as HTMLButtonElement;
    expect(pressed.dataset.pending).toBe('true');
    expect(pressed.disabled).toBe(true);
    expect(pressed.title).toBe('Retrying the job…');
    const other = within(rowActions('docs')).getByTestId('review-ci-job-retry_job') as HTMLButtonElement;
    expect(other.disabled).toBe(true);
    expect(other.dataset.pending).toBeUndefined();
    expect(other.title).toBe('Another action on this pipeline is still running.');
  });
});

describe('<ReviewCILogView> actions', () => {
  function renderLog(overrides: Record<string, unknown>) {
    return render(ReviewCILogView, {
      view: { stageName: 'test', jobId: '20', job: JOBS[0]! },
      log: null,
      fetchLines: async (from: number) => ({ from, lines: [], totalLines: 0, version: 0 }),
      loading: false,
      error: null,
      savedPath: null,
      openSections: new SvelteSet<string>(),
      onBack: () => {},
      onRefresh: () => {},
      onSave: () => {},
      onToggleSection: () => {},
      onSetSectionsOpen: () => {},
      onCopySection: async () => true,
      ...overrides,
    });
  }

  it('puts the job\'s actions in the header and says when it followed a retry', async () => {
    const { actions, run } = fakeActions(pipeline({}, JOBS));
    const { rerender } = renderLog({ actions });
    const header = screen.getByTestId('review-ci-log-actions');
    await fireEvent.click(within(header).getByTestId('review-ci-log-retry_job'));
    expect(run).toHaveBeenCalledWith('retry_job', JOBS[0]);
    expect(within(header).getByTestId('review-ci-log-artifacts')).toBeTruthy();
    expect(screen.queryByTestId('review-ci-log-retried')).toBeNull();
    await rerender({ retriedFrom: '19' });
    expect(screen.getByTestId('review-ci-log-retried').textContent).toContain('retried');
  });

  it('shows the download pending while it runs', () => {
    renderLog({ actions: fakeActions(pipeline({}, JOBS), null, true).actions });
    const download = screen.getByTestId('review-ci-log-artifacts') as HTMLButtonElement;
    expect(download.dataset.pending).toBe('true');
    expect(download.disabled).toBe(true);
  });
});
