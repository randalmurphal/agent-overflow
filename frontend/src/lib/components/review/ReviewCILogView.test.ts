import { fireEvent, within } from '@testing-library/dom';
import { flushSync, mount, tick, unmount } from 'svelte';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { SvelteSet } from 'svelte/reactivity';
import { rawProps } from '../../../test/helpers/rawProps.svelte';
import { resetScrollIntentModuleStateForTest } from '../../utils/scroll/intent';
import ReviewCILogView from './ReviewCILogView.svelte';
import { formatTimeOfDay } from '../../utils/format';
import type { ForgeFailure } from '../../utils/forgeFailure';
import type { CIJob } from '../../types/models';
import type { PRCILogState } from '../../stores/prReviewCI.svelte';
import { CI_LOG_BLOCK_LINES } from '../../stores/ciLogBlocks.svelte';
import { FakeCILog, numberedLines, section } from '../../../test/helpers/ciLog';

// The follow itself needs real geometry (a grown chunk re-measures through
// ResizeObserver, which happy-dom lacks): reviewCILogFollow.browser.test.ts
// proves it in Chromium and e2e/tests/review-ci-live.spec.ts end to end.
// These tests prove the intent wiring: a wheel away escapes the follow,
// and the controller writes through whichever scroller the log mounts,
// and the section rows: what they show, what their controls ask for,
// and the line rows: a block's lines, a placeholder until they load, the
// load's failure in their place.

// A job without steps or sections is one row, the job's; the follow
// tests keep it open.
const JOB: CIJob = {
  id: '20',
  name: 'unit',
  status: 'running',
  logsAvailable: true,
};

function props(fake: FakeCILog, overrides: Record<string, unknown> = {}) {
  return {
    view: { stageName: 'test', jobId: '20', job: JOB },
    log: fake.state as PRCILogState | null,
    fetchLines: fake.fetch,
    loading: false,
    error: null as string | null,
    failure: null as ForgeFailure | null,
    forge: 'github',
    savedPath: null,
    openSections: new SvelteSet<string>(['20/job', '21/job']),
    onBack: () => {},
    onRefresh: () => {},
    onSave: () => {},
    onSend: () => {},
    onToggleSection: () => {},
    onSetSectionsOpen: () => {},
    onCopySection: async () => true,
    onSendSection: () => {},
    ...overrides,
  };
}

async function settle(): Promise<void> {
  for (let i = 0; i < 6; i += 1) await tick();
  // The intent machine classifies a scroll event 1ms later.
  await new Promise((resolve) => setTimeout(resolve, 5));
}

const BOTTOM = 800;

/** Geometry happy-dom does not lay out, with every scrollTop write kept.
 * A 1000px log in a 200px viewport: the bottom is scrollTop 800. */
function stubGeometry(el: HTMLElement, at: { top: number }) {
  const writes: number[] = [];
  Object.defineProperty(el, 'scrollHeight', { configurable: true, get: () => 1000 });
  Object.defineProperty(el, 'clientHeight', { configurable: true, get: () => 200 });
  Object.defineProperty(el, 'scrollTop', {
    configurable: true,
    get: () => at.top,
    set: (value: number) => {
      writes.push(value);
      at.top = Math.max(0, Math.min(value, BOTTOM));
    },
  });
  return writes;
}

/** The reader wheels up and the scroller moves: the controller's escape. */
async function wheelUp(el: HTMLElement, at: { top: number }, to: number): Promise<void> {
  await fireEvent.wheel(el, { deltaY: -100 });
  at.top = to;
  await fireEvent.scroll(el);
  await settle();
}

const mounted: { app: object; host: HTMLElement }[] = [];

beforeEach(() => {
  resetScrollIntentModuleStateForTest();
});

afterEach(() => {
  for (const { app, host } of mounted.splice(0)) {
    void unmount(app);
    host.remove();
  }
});

// Each prop is its own signal, as ReviewPane passes them: a pipeline frame
// changes `view` and leaves `log` the same object.
function renderLogView(overrides: Record<string, unknown> = {}, fake = new FakeCILog(numberedLines(10))) {
  const host = document.createElement('div');
  document.body.appendChild(host);
  const p = rawProps(props(fake, overrides));
  const app = mount(ReviewCILogView, { target: host, props: p });
  mounted.push({ app, host });
  return { p, view: within(host), fake, host };
}

async function update(apply: () => void): Promise<void> {
  apply();
  flushSync();
  await settle();
}

describe('<ReviewCILogView>', () => {
  it('leaves a reader who wheeled up where they are when the log grows', async () => {
    const { p, view, fake } = renderLogView();
    await settle();
    const scroll = view.getByTestId('review-ci-log-scroll');
    const at = { top: BOTTOM };
    const writes = stubGeometry(scroll, at);
    await wheelUp(scroll, at, 300);
    writes.length = 0;

    await update(() => {
      fake.grow(numberedLines(10, 11));
      p.log = fake.state;
    });
    expect(view.getByTestId('review-ci-log-scroll')).toHaveTextContent('line 20');
    expect(writes).toEqual([]);
    expect(at.top).toBe(300);
  });

  it('places a newly opened job at its tail even for a reader who had wheeled up', async () => {
    const { p, view } = renderLogView();
    await settle();
    const scroll = view.getByTestId('review-ci-log-scroll');
    const at = { top: BOTTOM };
    const writes = stubGeometry(scroll, at);
    await wheelUp(scroll, at, 300);
    writes.length = 0;

    const other: CIJob = { ...JOB, id: '21', name: 'lint' };
    const lint = new FakeCILog(numberedLines(12));
    await update(() => {
      p.view = { stageName: 'test', jobId: '21', job: other };
      p.fetchLines = lint.fetch;
      p.log = lint.state;
    });
    // The placement is the virtualizer's jump, written by the controller.
    expect(writes.length).toBeGreaterThan(0);
  });

  it('does not move a reader who wheeled up when only the job row changes', async () => {
    const { p, view } = renderLogView();
    await settle();
    const scroll = view.getByTestId('review-ci-log-scroll');
    const at = { top: BOTTOM };
    const writes = stubGeometry(scroll, at);
    await wheelUp(scroll, at, 300);
    writes.length = 0;

    // A pipeline frame re-derives the view around the same log.
    await update(() => {
      p.view = { stageName: 'test', jobId: '20', job: { ...JOB, status: 'failed' } };
    });
    expect(writes).toEqual([]);
    expect(at.top).toBe(300);
    expect(view.getByText('failed')).toBeInTheDocument();
  });

  it('says when the log is available while a GitHub job runs, over its step list', async () => {
    const job: CIJob = {
      ...JOB,
      steps: [
        { number: 1, name: 'Set up job', status: 'success', startedAt: '2026-01-01T00:00:00Z', completedAt: '2026-01-01T00:00:02Z' },
        { number: 3, name: 'build', status: 'running', startedAt: '2026-01-01T00:00:02Z' },
        { number: 4, name: 'Post job', status: 'pending' },
      ],
    };
    // The backend lists the steps before there is a log.
    const fake = new FakeCILog([], [
      section('step:1', 'Set up job', 'success', 0, 0),
      section('step:3', 'build', 'running', 0, 0),
      section('step:4', 'Post job', 'pending', 0, 0),
    ]);
    fake.version = 0;
    fake.available = false;
    const { p, view } = renderLogView({ view: { stageName: 'test', jobId: '20', job } }, fake);
    await settle();
    expect(view.getByTestId('review-ci-log-pending')).toHaveTextContent('The log is available when the job completes.');
    expect(view.queryByTestId('review-ci-log-empty')).toBeNull();
    // The steps keep reporting the live job, with nothing to expand yet.
    const rows = view.getAllByTestId('review-ci-section');
    expect(rows.map((row) => [row.dataset.key, row.dataset.status])).toEqual([
      ['step:1', 'success'],
      ['step:3', 'running'],
      ['step:4', 'pending'],
    ]);
    for (const toggle of view.getAllByTestId('review-ci-section-toggle')) expect(toggle).toBeDisabled();
    expect(view.getByRole('button', { name: 'Refresh log' })).toBeInTheDocument();

    // A failed fetch is not "still running".
    await update(() => { p.error = 'failed to fetch job log (id: x)'; });
    expect(view.queryByTestId('review-ci-log-pending')).toBeNull();
    expect(view.getByTestId('review-ci-log-error')).toBeInTheDocument();
  });

  it('shows a rate-limited log as a pause until its resume time', async () => {
    const resumeAt = '2026-10-10T12:30:00Z';
    const { p, view } = renderLogView({
      error: 'failed to refresh pull request (id: x)',
      failure: { kind: 'rate_limited', reserve: true, resumeAt },
      forge: 'gitlab',
    });
    await settle();
    expect(view.getByTestId('review-ci-log-rate-limited')).toHaveTextContent(
      `GitLab rate limit nearly used up. Updates pause until ${formatTimeOfDay(Date.parse(resumeAt))} so your own actions still go through.`,
    );
    expect(view.queryByTestId('review-ci-log-error')).toBeNull();

    await update(() => { p.failure = { kind: 'forge', reserve: false, resumeAt: '' }; });
    expect(view.queryByTestId('review-ci-log-rate-limited')).toBeNull();
    expect(view.getByTestId('review-ci-log-error')).toHaveTextContent('failed to refresh pull request (id: x)');
  });

  it('says the forge has not published the log of a job that finished', async () => {
    const { p, view } = renderLogView({
      view: { stageName: 'test', jobId: '20', job: { ...JOB, status: 'pending' } },
      log: { ...unpublished(), sections: [] },
    });
    await settle();
    // Queued is live too.
    expect(view.getByTestId('review-ci-log-pending')).toHaveTextContent('The log is available when the job completes.');

    for (const status of ['failed', 'success', 'canceled']) {
      await update(() => {
        p.view = { stageName: 'test', jobId: '20', job: { ...JOB, status } };
      });
      expect(view.getByTestId('review-ci-log-pending')).toHaveTextContent('The job finished; the forge has not published its log yet.');
    }
    expect(view.queryByTestId('review-ci-log-error')).toBeNull();
    expect(view.queryByTestId('review-ci-log-empty')).toBeNull();
  });

  it('attaches the controller to each scroller the log mounts', async () => {
    // The scroller leaves with the rows and returns with them; the
    // controller writes through the second one.
    const { p, view, fake } = renderLogView({ log: null });
    await settle();
    expect(view.queryByTestId('review-ci-log-scroll')).toBeNull();
    await update(() => { p.log = fake.state; });
    await update(() => { p.log = null; });
    expect(view.queryByTestId('review-ci-log-scroll')).toBeNull();
    await update(() => { p.log = fake.state; });
    const at = { top: 0 };
    const writes = stubGeometry(view.getByTestId('review-ci-log-scroll'), at);
    const other: CIJob = { ...JOB, id: '21', name: 'lint' };
    const lint = new FakeCILog(numberedLines(12));
    await update(() => {
      p.view = { stageName: 'test', jobId: '21', job: other };
      p.fetchLines = lint.fetch;
      p.log = lint.state;
    });
    expect(writes.length).toBeGreaterThan(0);
  });

  it('says a cut download resumes, over the lines it has', async () => {
    const fake = new FakeCILog(numberedLines(3));
    fake.downloading = true;
    const { p, view } = renderLogView({}, fake);
    await settle();
    expect(view.getByTestId('review-ci-log-downloading')).toBeInTheDocument();
    expect(view.getByTestId('review-ci-log-scroll')).toHaveTextContent('line 3');
    fake.downloading = false;
    await update(() => { p.log = fake.state; });
    expect(view.queryByTestId('review-ci-log-downloading')).toBeNull();
  });
});

function unpublished(): PRCILogState {
  const fake = new FakeCILog([]);
  fake.version = 0;
  fake.available = false;
  return fake.state;
}

describe('<ReviewCILogView> line blocks', () => {
  it('shows a placeholder of the lines\' height until their block loads, then the lines', async () => {
    let open = () => {};
    const fake = new FakeCILog(numberedLines(CI_LOG_BLOCK_LINES + 50));
    fake.gate = new Promise((resolve) => { open = resolve; });
    const { view } = renderLogView({}, fake);
    await settle();
    // One row per block: the job's 250 lines are rows of 200 and 50.
    const holders = view.getAllByTestId('review-ci-log-block-placeholder');
    expect(holders.map((holder) => holder.style.height)).toEqual(['3600px', '900px']);
    expect(holders.map((holder) => holder.textContent?.trim())).toEqual(['Loading lines 1 to 200…', 'Loading lines 201 to 250…']);
    expect(fake.reads).toEqual([[0, CI_LOG_BLOCK_LINES], [CI_LOG_BLOCK_LINES, 50]]);

    open();
    await settle();
    expect(view.queryByTestId('review-ci-log-block-placeholder')).toBeNull();
    const scroll = view.getByTestId('review-ci-log-scroll');
    expect(scroll).toHaveTextContent('line 1 ');
    expect(scroll).toHaveTextContent(`line ${CI_LOG_BLOCK_LINES + 50}`);
    expect(view.getByTestId('review-ci-log').dataset.cachedBlocks).toBe('2');
  });
});

// A GitHub job's log as the forge serves it, every line with its time,
// and the steps the backend reads out of it.
const STEPPED: CIJob = {
  id: '20',
  name: 'unit',
  status: 'failed',
  logsAvailable: true,
};
const STEPPED_LOG = [
  '2026-01-01T00:00:00.1000000Z Current runner version: 2.337.0',
  '2026-01-01T00:00:01.2000000Z ##[group]Run make test',
  '2026-01-01T00:00:02.0000000Z \x1b[31mFAIL\x1b[0m pkg/a',
  '2026-01-01T00:01:05.1000000Z ##[error]Process completed with exit code 2.',
  '2026-01-01T00:01:05.2000000Z Cleaning up orphan processes',
];
const STEPS = [
  section('step:1', 'Set up job', 'success', 0, 1, { durationSeconds: 1 }),
  section('step:2', 'Run make test', 'failed', 1, 4, { durationSeconds: 64 }),
  section('step:4', 'Complete job', 'success', 4, 5),
];

type Queries = ReturnType<typeof within>;

function sectionRows(view: Queries): HTMLElement[] {
  return view.getAllByTestId('review-ci-section');
}

function sectionRow(view: Queries, key: string): HTMLElement {
  const row = sectionRows(view).find((candidate) => candidate.dataset.key === key);
  if (!row) throw new Error(`no row ${key}`);
  return row;
}

function renderStepped(overrides: Record<string, unknown> = {}, steps = STEPS) {
  return renderLogView({
    view: { stageName: 'CI', jobId: '20', job: STEPPED },
    openSections: new SvelteSet<string>(),
    ...overrides,
  }, new FakeCILog(STEPPED_LOG, steps));
}

describe('<ReviewCILogView> sections', () => {
  it('lists the rows collapsed, with status and duration, and reads no lines', async () => {
    const { view, fake } = renderStepped();
    await settle();
    expect(sectionRows(view).map((row) => [row.dataset.key, row.dataset.status, row.dataset.open])).toEqual([
      ['step:1', 'success', 'false'],
      ['step:2', 'failed', 'false'],
      ['step:4', 'success', 'false'],
    ]);
    expect(sectionRow(view, 'step:2')).toHaveTextContent('Run make test');
    expect(sectionRow(view, 'step:2')).toHaveTextContent('1m 4s');
    // Nothing expands on its own, not even the failed step.
    expect(view.getByTestId('review-ci-log-scroll')).not.toHaveTextContent('FAIL');
    expect(fake.reads).toEqual([]);
  });

  it('asks to toggle a row and shows only an open row\'s lines', async () => {
    const onToggleSection = vi.fn();
    const openSections = new SvelteSet<string>();
    const { view, fake } = renderStepped({ onToggleSection, openSections });
    await settle();
    await fireEvent.click(within(sectionRow(view, 'step:2')).getByTestId('review-ci-section-toggle'));
    await settle();
    expect(onToggleSection).toHaveBeenCalledWith('step:2');

    openSections.add('20/step:2');
    flushSync();
    await settle();
    const scroll = view.getByTestId('review-ci-log-scroll');
    expect(sectionRow(view, 'step:2').dataset.open).toBe('true');
    expect(within(sectionRow(view, 'step:2')).getByTestId('review-ci-section-toggle')).toHaveAttribute('aria-expanded', 'true');
    expect(scroll).toHaveTextContent('FAIL pkg/a');
    expect(scroll).toHaveTextContent('##[error]Process completed with exit code 2.');
    expect(scroll).not.toHaveTextContent('Current runner version');
    expect(scroll).not.toHaveTextContent('Cleaning up orphan processes');
    // The open row reads its own lines only.
    expect(fake.reads).toEqual([[0, 5]]);
  });

  it('expands every row with lines, then collapses them, from one control', async () => {
    const onSetSectionsOpen = vi.fn();
    const openSections = new SvelteSet<string>();
    // A step not reached yet has nothing to expand.
    const { view } = renderStepped({ onSetSectionsOpen, openSections }, [...STEPS, section('step:5', 'Upload', 'pending', 5, 5)]);
    await settle();
    expect(sectionRow(view, 'step:5').dataset.status).toBe('pending');
    const control = view.getByTestId('review-ci-log-expand-all');
    expect(control).toHaveAttribute('aria-label', 'Expand all');
    await fireEvent.click(control);
    expect(onSetSectionsOpen).toHaveBeenLastCalledWith(['step:1', 'step:2', 'step:4'], true);

    for (const key of ['step:1', 'step:2', 'step:4']) openSections.add(`20/${key}`);
    flushSync();
    await settle();
    expect(control).toHaveAttribute('aria-label', 'Collapse all');
    await fireEvent.click(control);
    expect(onSetSectionsOpen).toHaveBeenLastCalledWith(['step:1', 'step:2', 'step:4'], false);
  });

  it('asks to send one section', async () => {
    const onSendSection = vi.fn();
    const { view } = renderStepped({ onSendSection });
    await settle();
    await fireEvent.click(within(sectionRow(view, 'step:2')).getByTestId('review-ci-section-send'));
    expect(onSendSection).toHaveBeenCalledWith(STEPS[1]);
  });

  it('offers no Send to chat for a review with no chat', async () => {
    const { view } = renderStepped({ onSend: undefined, onSendSection: undefined });
    await settle();
    expect(view.queryByTestId('review-ci-log-send')).toBeNull();
    expect(within(sectionRow(view, 'step:2')).queryByTestId('review-ci-section-send')).toBeNull();
    // Copy stays: it needs no chat.
    expect(within(sectionRow(view, 'step:2')).getByTestId('review-ci-section-copy')).toBeInTheDocument();
  });

  it('shows a running GitLab section with its line count', async () => {
    const job: CIJob = { id: '20', name: 'unit', status: 'running', logsAvailable: true };
    const fake = new FakeCILog(['Preparing the docker executor', 'Executing step_script', '$ make test'], [
      section('section:prepare_executor:1700000000', 'prepare_executor', 'done', 0, 1, { durationSeconds: 4 }),
      section('section:step_script:1700000004', 'step_script', 'running', 1, 3),
    ]);
    const { view } = renderLogView({
      view: { stageName: 'test', jobId: '20', job },
      forge: 'gitlab',
      openSections: new SvelteSet<string>(['20/section:prepare_executor:1700000000', '20/section:step_script:1700000004']),
    }, fake);
    await settle();
    expect(sectionRows(view).map((row) => [row.dataset.status, row.textContent?.trim().replace(/\s+/g, ' ')])).toEqual([
      ['done', 'prepare_executor 4s'],
      ['running', 'step_script 2 lines'],
    ]);
    const scroll = view.getByTestId('review-ci-log-scroll');
    expect(scroll).toHaveTextContent('Preparing the docker executor');
    expect(scroll).toHaveTextContent('$ make test');
  });

  it('says a GitLab trace is loading while the forge has none for a running job', async () => {
    const { view } = renderLogView({ log: unpublished(), forge: 'gitlab' });
    await settle();
    expect(view.getByTestId('review-ci-log-pending')).toHaveTextContent('The trace is loading.');
  });
});

describe('<ReviewCILogView> section copy', () => {
  it('asks to copy a section and shows Copied once it is, until the swap resets', async () => {
    vi.useFakeTimers();
    try {
      const onCopySection = vi.fn(async () => true);
      const { view } = renderStepped({ onCopySection });
      await vi.advanceTimersByTimeAsync(10);
      await fireEvent.click(within(sectionRow(view, 'step:4')).getByTestId('review-ci-section-copy'));
      await vi.advanceTimersByTimeAsync(0);
      expect(onCopySection).toHaveBeenCalledWith(STEPS[2]);
      expect(within(sectionRow(view, 'step:4')).getByTestId('review-ci-section-copy')).toHaveAttribute('aria-label', 'Copied');
      expect(within(sectionRow(view, 'step:1')).getByTestId('review-ci-section-copy')).toHaveAttribute('aria-label', 'Copy section');
      await vi.advanceTimersByTimeAsync(2000);
      expect(within(sectionRow(view, 'step:4')).getByTestId('review-ci-section-copy')).toHaveAttribute('aria-label', 'Copy section');
    } finally {
      vi.useRealTimers();
    }
  });

  it('does not show Copied when the copy failed', async () => {
    const onCopySection = vi.fn(async () => false);
    const { view } = renderStepped({ onCopySection });
    await settle();
    await fireEvent.click(within(sectionRow(view, 'step:2')).getByTestId('review-ci-section-copy'));
    await settle();
    expect(onCopySection).toHaveBeenCalledTimes(1);
    expect(within(sectionRow(view, 'step:2')).getByTestId('review-ci-section-copy')).toHaveAttribute('aria-label', 'Copy section');
  });
});
