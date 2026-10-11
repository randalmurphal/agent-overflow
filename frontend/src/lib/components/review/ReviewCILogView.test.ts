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
import { getToasts } from '../../stores/toast.svelte';

// The follow itself needs real geometry (a grown chunk re-measures through
// ResizeObserver, which happy-dom lacks): reviewCILogFollow.browser.test.ts
// proves it in Chromium and e2e/tests/review-ci-live.spec.ts end to end.
// These tests prove the intent wiring: a wheel away escapes the follow,
// and the controller writes through whichever scroller the log mounts,
// and the section rows: what they show, what their controls ask for.

// A job without steps or sections is one row, the job's; the follow
// tests keep it open.
const JOB: CIJob = {
  id: '20',
  name: 'unit',
  status: 'running',
  logsAvailable: true,
};

function logOf(lines: number) {
  const text = Array.from({ length: lines }, (_, index) => `line ${index + 1}`).join('\n') + '\n';
  return { text, truncated: false, totalBytes: text.length };
}

function props(overrides: Record<string, unknown> = {}) {
  return {
    view: { stageName: 'test', jobId: '20', job: JOB },
    log: logOf(10) as ReturnType<typeof logOf> | null,
    loading: false,
    error: null as string | null,
    failure: null as ForgeFailure | null,
    forge: 'github',
    available: true,
    savedPath: null,
    openSections: new SvelteSet<string>(['20/job', '21/job']),
    onBack: () => {},
    onRefresh: () => {},
    onSave: () => {},
    onSend: () => {},
    onToggleSection: () => {},
    onSetSectionsOpen: () => {},
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
function renderLogView(overrides: Record<string, unknown> = {}) {
  const host = document.createElement('div');
  document.body.appendChild(host);
  const p = rawProps(props(overrides));
  const app = mount(ReviewCILogView, { target: host, props: p });
  mounted.push({ app, host });
  return { p, view: within(host) };
}

async function update(apply: () => void): Promise<void> {
  apply();
  flushSync();
  await settle();
}

describe('<ReviewCILogView>', () => {
  it('leaves a reader who wheeled up where they are when the log grows', async () => {
    const { p, view } = renderLogView();
    await settle();
    const scroll = view.getByTestId('review-ci-log-scroll');
    const at = { top: BOTTOM };
    const writes = stubGeometry(scroll, at);
    await wheelUp(scroll, at, 300);
    writes.length = 0;

    await update(() => { p.log = logOf(20); });
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
    await update(() => {
      p.view = { stageName: 'test', jobId: '21', job: other };
      p.log = logOf(12);
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
    const { p, view } = renderLogView({
      view: { stageName: 'test', jobId: '20', job },
      log: { text: '', truncated: false, totalBytes: 0 },
      available: false,
    });
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
      log: logOf(2),
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
      log: { text: '', truncated: false, totalBytes: 0 },
      available: false,
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
    // The scroller leaves with the text and returns with it; the
    // controller writes through the second one.
    const { p, view } = renderLogView({ log: null });
    await settle();
    expect(view.queryByTestId('review-ci-log-scroll')).toBeNull();
    await update(() => { p.log = logOf(10); });
    await update(() => { p.log = null; });
    expect(view.queryByTestId('review-ci-log-scroll')).toBeNull();
    await update(() => { p.log = logOf(10); });
    const at = { top: 0 };
    const writes = stubGeometry(view.getByTestId('review-ci-log-scroll'), at);
    const other: CIJob = { ...JOB, id: '21', name: 'lint' };
    await update(() => {
      p.view = { stageName: 'test', jobId: '21', job: other };
      p.log = logOf(12);
    });
    expect(writes.length).toBeGreaterThan(0);
  });
});

// A GitHub job's log as the forge serves it: every line has its time.
const STEPPED: CIJob = {
  id: '20',
  name: 'unit',
  status: 'failed',
  logsAvailable: true,
  steps: [
    { number: 1, name: 'Set up job', status: 'success', startedAt: '2026-01-01T00:00:00Z', completedAt: '2026-01-01T00:00:01Z' },
    { number: 2, name: 'Run make test', status: 'failed', startedAt: '2026-01-01T00:00:01Z', completedAt: '2026-01-01T00:01:05Z' },
    { number: 3, name: 'Run make lint', status: 'skipped', startedAt: '2026-01-01T00:01:05Z', completedAt: '2026-01-01T00:01:05Z' },
    { number: 4, name: 'Complete job', status: 'success', startedAt: '2026-01-01T00:01:05Z', completedAt: '2026-01-01T00:01:05Z' },
  ],
};
const STEPPED_LOG = [
  '2026-01-01T00:00:00.1000000Z Current runner version: 2.337.0',
  '2026-01-01T00:00:01.2000000Z ##[group]Run make test',
  '2026-01-01T00:00:02.0000000Z \x1b[31mFAIL\x1b[0m pkg/a',
  '2026-01-01T00:01:05.1000000Z ##[error]Process completed with exit code 2.',
  '2026-01-01T00:01:05.2000000Z Cleaning up orphan processes',
  '',
].join('\n');

const GITLAB_LOG = [
  'section_start:1700000000:prepare_executor',
  'Preparing the docker executor',
  'section_end:1700000004:prepare_executor',
  'section_start:1700000004:step_script',
  'Executing step_script',
  '$ make test',
  '',
].join('\n');

type Queries = ReturnType<typeof within>;

function sectionRows(view: Queries): HTMLElement[] {
  return view.getAllByTestId('review-ci-section');
}

function sectionRow(view: Queries, key: string): HTMLElement {
  const row = sectionRows(view).find((candidate) => candidate.dataset.key === key);
  if (!row) throw new Error(`no row ${key}`);
  return row;
}

function renderStepped(overrides: Record<string, unknown> = {}) {
  return renderLogView({
    view: { stageName: 'CI', jobId: '20', job: STEPPED },
    log: { text: STEPPED_LOG, truncated: false, totalBytes: STEPPED_LOG.length },
    openSections: new SvelteSet<string>(),
    ...overrides,
  });
}

describe('<ReviewCILogView> sections', () => {
  it('lists the steps collapsed, with status and duration, and no skipped step', async () => {
    const { view } = renderStepped();
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
  });

  it('asks to toggle a row and shows only an open row\'s lines', async () => {
    const onToggleSection = vi.fn();
    const openSections = new SvelteSet<string>();
    const { view } = renderStepped({ onToggleSection, openSections });
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
  });

  it('expands every row with lines, then collapses them, from one control', async () => {
    const onSetSectionsOpen = vi.fn();
    const openSections = new SvelteSet<string>();
    // A step not reached yet has nothing to expand.
    const job: CIJob = { ...STEPPED, steps: [...STEPPED.steps!, { number: 5, name: 'Upload', status: 'pending' }] };
    const { view } = renderStepped({ view: { stageName: 'CI', jobId: '20', job }, onSetSectionsOpen, openSections });
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

  it('sends one section as plain text', async () => {
    const onSendSection = vi.fn();
    const { view } = renderStepped({ onSendSection });
    await settle();
    await fireEvent.click(within(sectionRow(view, 'step:2')).getByTestId('review-ci-section-send'));
    expect(onSendSection).toHaveBeenCalledWith({
      name: 'Run make test',
      status: 'failed',
      text: [
        '2026-01-01T00:00:01.2000000Z ##[group]Run make test',
        '2026-01-01T00:00:02.0000000Z FAIL pkg/a',
        '2026-01-01T00:01:05.1000000Z ##[error]Process completed with exit code 2.',
      ].join('\n'),
      truncatedTop: false,
    });
  });

  it('marks the row a cut log starts partway through', async () => {
    const tail = STEPPED_LOG.split('\n').slice(2).join('\n');
    const { view } = renderStepped({
      log: { text: tail, truncated: true, totalBytes: 3 * 1024 * 1024 },
      openSections: new SvelteSet<string>(['20/step:2']),
    });
    await settle();
    expect(view.getByTestId('review-ci-log-truncated')).toHaveTextContent('Showing the tail of a 3.0 MB log. Save to file for the full log.');
    expect(view.getAllByTestId('review-ci-section-cut')).toHaveLength(1);
    expect(within(sectionRow(view, 'step:1')).getByTestId('review-ci-section-toggle')).toBeDisabled();
  });

  it('shows GitLab sections without their markers, and a running one with its line count', async () => {
    const job: CIJob = { id: '20', name: 'unit', status: 'running', logsAvailable: true };
    const { view } = renderLogView({
      view: { stageName: 'test', jobId: '20', job },
      log: { text: GITLAB_LOG, truncated: false, totalBytes: GITLAB_LOG.length },
      forge: 'gitlab',
      openSections: new SvelteSet<string>(['20/section:prepare_executor:1700000000', '20/section:step_script:1700000004']),
    });
    await settle();
    expect(sectionRows(view).map((row) => [row.dataset.status, row.textContent?.trim().replace(/\s+/g, ' ')])).toEqual([
      ['done', 'Preparing the docker executor 4s'],
      ['running', 'Executing step_script 2 lines'],
    ]);
    const scroll = view.getByTestId('review-ci-log-scroll');
    expect(scroll).toHaveTextContent('$ make test');
    expect(scroll).not.toHaveTextContent('section_');
  });

  it('says a GitLab trace is loading while the forge has none for a running job', async () => {
    const { view } = renderLogView({
      log: { text: '', truncated: false, totalBytes: 0 },
      available: false,
      forge: 'gitlab',
    });
    await settle();
    expect(view.getByTestId('review-ci-log-pending')).toHaveTextContent('The trace is loading.');
  });
});

// happy-dom has no clipboard; the stub is an own property the shared-worker
// guard expects gone (not present as undefined) when the file ends.
const originalClipboard = Object.getOwnPropertyDescriptor(navigator, 'clipboard');

function stubClipboard(writeText: (text: string) => Promise<void>) {
  Object.defineProperty(navigator, 'clipboard', { value: { writeText }, configurable: true, writable: true });
}

describe('<ReviewCILogView> section copy', () => {
  afterEach(() => {
    delete (navigator as { clipboard?: unknown }).clipboard;
    if (originalClipboard) Object.defineProperty(navigator, 'clipboard', originalClipboard);
  });

  it('copies the section as plain text and shows Copied until the swap resets', async () => {
    vi.useFakeTimers();
    try {
      const writeText = vi.fn(async () => {});
      stubClipboard(writeText);
      const { view } = renderStepped();
      await vi.advanceTimersByTimeAsync(10);
      await fireEvent.click(within(sectionRow(view, 'step:4')).getByTestId('review-ci-section-copy'));
      await vi.advanceTimersByTimeAsync(0);
      expect(writeText).toHaveBeenCalledWith('2026-01-01T00:01:05.2000000Z Cleaning up orphan processes');
      expect(within(sectionRow(view, 'step:4')).getByTestId('review-ci-section-copy')).toHaveAttribute('aria-label', 'Copied');
      expect(within(sectionRow(view, 'step:1')).getByTestId('review-ci-section-copy')).toHaveAttribute('aria-label', 'Copy section');
      await vi.advanceTimersByTimeAsync(2000);
      expect(within(sectionRow(view, 'step:4')).getByTestId('review-ci-section-copy')).toHaveAttribute('aria-label', 'Copy section');
    } finally {
      vi.useRealTimers();
    }
  });

  it('raises a toast when the clipboard refuses', async () => {
    const before = getToasts().length;
    stubClipboard(async () => { throw new DOMException('denied', 'NotAllowedError'); });
    const { view } = renderStepped();
    await settle();
    await fireEvent.click(within(sectionRow(view, 'step:2')).getByTestId('review-ci-section-copy'));
    await vi.waitFor(() => expect(getToasts().length).toBe(before + 1));
    expect(getToasts().at(-1)?.message).toBe('Failed to copy');
  });
});
