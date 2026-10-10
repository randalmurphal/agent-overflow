import { fireEvent, within } from '@testing-library/dom';
import { flushSync, mount, tick, unmount } from 'svelte';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { rawProps } from '../../../test/helpers/rawProps.svelte';
import { resetScrollIntentModuleStateForTest } from '../../utils/scroll/intent';
import ReviewCILogView from './ReviewCILogView.svelte';
import { formatTimeOfDay } from '../../utils/format';
import type { ForgeFailure } from '../../utils/forgeFailure';
import type { CIJob } from '../../types/models';

// The follow itself needs real geometry (a grown chunk re-measures through
// ResizeObserver, which happy-dom lacks): reviewCILogFollow.browser.test.ts
// proves it in Chromium and e2e/tests/review-ci-live.spec.ts end to end.
// These tests prove the intent wiring: a wheel away escapes the follow,
// and the controller writes through whichever scroller the log mounts.

const JOB: CIJob = {
  id: '20',
  name: 'unit',
  status: 'running',
  logsAvailable: true,
  steps: [{ number: 1, name: 'build', status: 'running' }],
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
    onBack: () => {},
    onRefresh: () => {},
    onSave: () => {},
    onSend: () => {},
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

  it('says when the log is available while the job runs on a forge without live logs', async () => {
    const { p, view } = renderLogView({ log: { text: '', truncated: false, totalBytes: 0 }, available: false });
    await settle();
    expect(view.getByTestId('review-ci-log-pending')).toHaveTextContent('The log is available when the job completes.');
    expect(view.queryByTestId('review-ci-log-empty')).toBeNull();
    // The steps keep reporting the live job.
    expect(view.getByTitle('build: running')).toBeInTheDocument();
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
