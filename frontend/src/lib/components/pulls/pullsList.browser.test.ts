// Real-Chromium suite for the page's virtualized lists: a list of
// thousands of merge requests or pipelines mounts the rows near the
// viewport only, reports the rows on screen by key as it scrolls (none
// while hidden), keeps the group under the viewport top named, and keeps
// Tab walking the rows past the mounted window.

import { afterEach, describe, expect, it } from 'vitest';
import { userEvent } from 'vitest/browser';
import { flushSync, mount, unmount } from 'svelte';
import '../../../app.css';
import PullsList from './PullsList.svelte';
import PipelinesList from './PipelinesList.svelte';
import type { PullRowView } from './pullRowView';
import type { PipelineRowView } from './pipelineRowView';
import { raf, waitFor } from '../../../test/helpers/browserFrames';
import { rawProps } from '../../../test/helpers/rawProps.svelte';

const VIEWPORT_PX = 600;
const NEEDS_YOU = 2_000;
const WAITING = 3_000;
// The viewport plus the virtualizer's overscan on each side, in rows of
// at least 60px, with slack: far below the thousands listed.
const MOUNTED_BOUND = 80;

const mounted: { app: object; host: HTMLElement }[] = [];

afterEach(() => {
  for (const { app, host } of mounted.splice(0)) {
    unmount(app);
    host.remove();
  }
});

function pullRow(index: number, group: string): PullRowView {
  return {
    key: `local:gitlab.com/ao/widgets:${index}`,
    forge: 'gitlab',
    number: index,
    title: `Merge request ${index}`,
    url: `https://gitlab.com/ao/widgets/-/merge_requests/${index}`,
    state: group === 'needs_you' ? 'your_turn' : 'open',
    group,
    since: null,
    seen: false,
    projectName: 'widgets',
    author: 'alice',
    age: '2h ago',
    draft: false,
    crossRepository: false,
    ci: 'success',
    approvals: null,
    unresolved: null,
  };
}

function pullGroups() {
  const needsYou = Array.from({ length: NEEDS_YOU }, (_, index) => pullRow(index, 'needs_you'));
  const waiting = Array.from({ length: WAITING }, (_, index) => pullRow(NEEDS_YOU + index, 'waiting'));
  return [
    { group: 'needs_you', rows: needsYou },
    { group: 'waiting', rows: waiting },
  ];
}

function makeHost(): HTMLElement {
  const host = document.createElement('div');
  host.style.cssText = `position: fixed; top: 0; left: 0; width: 400px; height: ${VIEWPORT_PX}px; display: flex; flex-direction: column;`;
  document.body.appendChild(host);
  return host;
}

function mountPulls() {
  const host = makeHost();
  const reports: string[][] = [];
  const props = rawProps({
    groups: pullGroups(),
    projectErrors: [] as { projectId: string; name: string; message: string; failure: null; forge: string }[],
    selectedKey: null as string | null,
    filter: 'all' as const,
    noProjects: false,
    error: null as string | null,
    loading: false,
    retrying: false,
    shown: true,
    onOpen: () => {},
    onMenu: () => {},
    onRetry: () => {},
    onVisible: (keys: string[]) => {
      reports.push(keys);
    },
  });
  const app = mount(PullsList, { target: host, props });
  mounted.push({ app, host });
  const scrollEl = host.querySelector<HTMLElement>('[data-testid="pulls-list"]')!;
  return { host, props, reports, scrollEl };
}

/** Geometry stable for three frames: moves, measurements and their
 * scroll events have all landed. */
async function settle(scrollEl: HTMLElement): Promise<void> {
  let last = '';
  let stable = 0;
  await waitFor(() => {
    const now = `${scrollEl.scrollTop}:${scrollEl.scrollHeight}`;
    stable = now === last ? stable + 1 : 0;
    last = now;
    return stable >= 3;
  }, 'stable geometry');
}

function mountedKeys(scrollEl: HTMLElement, attr: string): string[] {
  return [...scrollEl.querySelectorAll<HTMLElement>(`[${attr}]`)].map((el) => el.getAttribute(attr)!);
}

/** The keys of the mounted rows that intersect the scroller's box. */
function onScreenKeys(scrollEl: HTMLElement, attr: string): string[] {
  const box = scrollEl.getBoundingClientRect();
  return [...scrollEl.querySelectorAll<HTMLElement>(`[${attr}]`)]
    .filter((el) => {
      const rect = el.getBoundingClientRect();
      return rect.bottom > box.top && rect.top < box.bottom;
    })
    .map((el) => el.getAttribute(attr)!);
}

function lastReport(reports: string[][]): string[] {
  return reports[reports.length - 1] ?? [];
}

function stuckHeading(host: HTMLElement): string | null {
  return host.querySelector('[data-testid="pulls-list-stuck-heading"]')?.textContent?.replace(/\s+/g, ' ').trim() ?? null;
}

describe('PullsList', () => {
  it('mounts the rows near the viewport of thousands and reports the rows on screen', async () => {
    const { reports, scrollEl } = mountPulls();
    await settle(scrollEl);
    await waitFor(() => lastReport(reports).length > 0, 'a visible report');

    const keys = mountedKeys(scrollEl, 'data-pr-key');
    expect(keys.length).toBeGreaterThan(0);
    expect(keys.length).toBeLessThan(MOUNTED_BOUND);
    expect(new Set(lastReport(reports))).toEqual(new Set(onScreenKeys(scrollEl, 'data-pr-key')));
    expect(lastReport(reports)[0]).toBe(pullRow(0, 'needs_you').key);

    // Scrolling to the end renders the last row and reports it on screen.
    scrollEl.scrollTop = scrollEl.scrollHeight;
    await settle(scrollEl);
    const lastKey = pullRow(NEEDS_YOU + WAITING - 1, 'waiting').key;
    await waitFor(() => lastReport(reports).includes(lastKey), 'the last row reported');
    expect(mountedKeys(scrollEl, 'data-pr-key')).toContain(lastKey);
    expect(mountedKeys(scrollEl, 'data-pr-key').length).toBeLessThan(MOUNTED_BOUND);
    expect(new Set(lastReport(reports))).toEqual(new Set(onScreenKeys(scrollEl, 'data-pr-key')));
    expect(lastReport(reports)).not.toContain(pullRow(0, 'needs_you').key);
  });

  it('names the group under the viewport top once its heading scrolls under it', async () => {
    const { host, scrollEl } = mountPulls();
    await settle(scrollEl);
    expect(stuckHeading(host)).toBeNull();

    scrollEl.scrollTop = 5_000;
    await settle(scrollEl);
    await waitFor(() => stuckHeading(host) !== null, 'a stuck heading');
    expect(stuckHeading(host)).toBe(`Needs you ${NEEDS_YOU}`);
    // Pinned at the list's top inside the scroller, as a sticky heading:
    // a press there lands on it, not on the row under it, and a wheel
    // over it scrolls the list.
    const stuckEl = host.querySelector<HTMLElement>('[data-testid="pulls-list-stuck-heading"]')!;
    const box = scrollEl.getBoundingClientRect();
    expect(stuckEl.getBoundingClientRect().top).toBe(box.top + scrollEl.clientTop);
    expect(scrollEl.contains(stuckEl)).toBe(true);
    const hit = document.elementFromPoint(box.left + 20, box.top + 5);
    expect(stuckEl.contains(hit)).toBe(true);

    scrollEl.scrollTop = scrollEl.scrollHeight;
    await settle(scrollEl);
    await waitFor(() => stuckHeading(host)?.startsWith('Waiting') ?? false, 'the second group named');
    expect(stuckHeading(host)).toBe(`Waiting on others ${WAITING}`);

    scrollEl.scrollTop = 0;
    await settle(scrollEl);
    await waitFor(() => stuckHeading(host) === null, 'no stuck heading at the top');
  });

  it('reports no row while hidden, and the rows on screen again once shown', async () => {
    const { host, props, reports, scrollEl } = mountPulls();
    await settle(scrollEl);
    scrollEl.scrollTop = 40_000;
    await settle(scrollEl);
    await waitFor(() => lastReport(reports).length > 0 && !lastReport(reports).includes(pullRow(0, 'needs_you').key), 'mid-list rows reported');
    const before = lastReport(reports);
    const top = scrollEl.scrollTop;

    // The compact detail hides the list column with display: none.
    host.style.display = 'none';
    props.shown = false;
    flushSync();
    await raf();
    expect(lastReport(reports)).toEqual([]);

    host.style.display = 'flex';
    props.shown = true;
    flushSync();
    await settle(scrollEl);
    await waitFor(() => lastReport(reports).length > 0, 'rows reported again');
    expect(scrollEl.scrollTop).toBe(top);
    expect(lastReport(reports)).toEqual(before);
    expect(new Set(lastReport(reports))).toEqual(new Set(onScreenKeys(scrollEl, 'data-pr-key')));
  });

  it('reports the rows on screen when the rows change under a still viewport', async () => {
    const { props, reports, scrollEl } = mountPulls();
    props.groups = [{ group: 'needs_you', rows: Array.from({ length: 20 }, (_, index) => pullRow(index, 'needs_you')) }];
    flushSync();
    await settle(scrollEl);
    await waitFor(() => lastReport(reports)[0] === pullRow(0, 'needs_you').key, 'the first rows reported');

    // The same rows re-sorted: every height is measured and unchanged, so
    // nothing but the data says the rows on screen are others now.
    props.groups = [{ group: 'needs_you', rows: [...props.groups[0].rows].reverse() }];
    flushSync();
    await settle(scrollEl);
    expect(lastReport(reports)[0]).toBe(pullRow(19, 'needs_you').key);
    expect(new Set(lastReport(reports))).toEqual(new Set(onScreenKeys(scrollEl, 'data-pr-key')));

    props.groups = [];
    flushSync();
    await settle(scrollEl);
    expect(lastReport(reports)).toEqual([]);
  });

  it('walks rows with Tab past the mounted window', async () => {
    const { scrollEl } = mountPulls();
    await settle(scrollEl);
    const initial = mountedKeys(scrollEl, 'data-pr-key');
    const target = initial.length + 10;
    scrollEl.querySelector<HTMLElement>(`[data-pr-key="${pullRow(0, 'needs_you').key}"] button`)!.focus();
    // Each row is two tab stops: the row and its menu.
    for (let stop = 0; stop < target * 2; stop += 1) {
      await userEvent.keyboard('{Tab}');
    }
    const focused = document.activeElement?.closest<HTMLElement>('[data-pr-key]');
    expect(focused?.dataset.prKey).toBe(pullRow(target, 'needs_you').key);
    expect(mountedKeys(scrollEl, 'data-pr-key').length).toBeLessThan(MOUNTED_BOUND);
  });
});

function pipelineRow(index: number): PipelineRowView {
  return {
    key: `pipeline:gitlab.com/ao/widgets:${index}`,
    id: String(index),
    status: 'success',
    name: '',
    ref: 'main',
    refKind: 'branch',
    mrNumber: 0,
    forge: 'gitlab',
    shortSHA: 'abc1234',
    title: `Pipeline ${index}`,
    projectName: 'widgets',
    age: '1h ago',
    stagesError: null,
    url: '',
    stages: null,
  };
}

describe('PipelinesList', () => {
  it('mounts the rows near the viewport of thousands and reports the last row once scrolled to', async () => {
    const host = makeHost();
    const reports: string[][] = [];
    const rows = Array.from({ length: 5_000 }, (_, index) => pipelineRow(index));
    const app = mount(PipelinesList, {
      target: host,
      props: {
        rows,
        projectErrors: [{ projectId: 'p1', name: 'gadgets', message: 'The forge refused the list.', failure: null, forge: 'github' }],
        selectedKey: null,
        error: null,
        loading: false,
        retrying: false,
        shown: true,
        onOpen: () => {},
        onRetry: () => {},
        onVisible: (keys: string[]) => {
          reports.push(keys);
        },
      },
    });
    mounted.push({ app, host });
    const scrollEl = host.querySelector<HTMLElement>('[data-testid="pipelines-list"]')!;
    await settle(scrollEl);
    await waitFor(() => lastReport(reports).length > 0, 'a visible report');
    expect(scrollEl.querySelector('[data-testid="pipelines-project-group"]')).not.toBeNull();
    expect(mountedKeys(scrollEl, 'data-pipeline-key').length).toBeLessThan(MOUNTED_BOUND);
    expect(new Set(lastReport(reports))).toEqual(new Set(onScreenKeys(scrollEl, 'data-pipeline-key')));

    scrollEl.scrollTop = scrollEl.scrollHeight;
    await settle(scrollEl);
    await waitFor(() => lastReport(reports).includes(pipelineRow(4_999).key), 'the last row reported');
    expect(mountedKeys(scrollEl, 'data-pipeline-key')).toContain(pipelineRow(4_999).key);
    expect(mountedKeys(scrollEl, 'data-pipeline-key').length).toBeLessThan(MOUNTED_BOUND);
    expect(new Set(lastReport(reports))).toEqual(new Set(onScreenKeys(scrollEl, 'data-pipeline-key')));
    // Below the failed project's section, no heading is drawn over the rows.
    expect(host.querySelector('[data-testid="pulls-list-stuck-heading"]')).toBeNull();
  });
});
