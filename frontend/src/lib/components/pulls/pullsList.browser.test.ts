// Real-Chromium suite for the page's virtualized lists: a list of
// thousands of merge requests or pipelines mounts the rows near the
// viewport only, keeps the group under the viewport top named, keeps Tab
// walking the rows past the mounted window, and (the Pipelines list,
// whose store reads stages for the rows on screen) reports the rows on
// screen by key as it scrolls, none while hidden or once unmounted.

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
    label: group === 'needs_you' ? 'Review requested' : 'Open',
    tone: group === 'needs_you' ? 'accent' : 'muted',
    projectName: 'widgets',
    author: 'alice',
    viewerIsAuthor: false,
    age: '2h',
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
  const props = rawProps({
    groups: pullGroups(),
    projectErrors: [] as { projectId: string; name: string; message: string; failure: null; forge: string }[],
    selectedKey: null as string | null,
    filter: 'all' as const,
    orderKey: 'updated:forward',
    noProjects: false,
    error: null as string | null,
    loading: false,
    retrying: false,
    shown: true,
    onOpen: () => {},
    onMenu: () => {},
    onRetry: () => {},
  });
  const app = mount(PullsList, { target: host, props });
  mounted.push({ app, host });
  const scrollEl = host.querySelector<HTMLElement>('[data-testid="pulls-list"]')!;
  return { host, props, scrollEl };
}

function mountPipelines(rows: PipelineRowView[], projectErrors: { projectId: string; name: string; message: string; failure: null; forge: string }[] = []) {
  const host = makeHost();
  const reports: string[][] = [];
  const props = rawProps({
    rows,
    projectErrors,
    selectedKey: null as string | null,
    error: null as string | null,
    loading: false,
    retrying: false,
    shown: true,
    onOpen: () => {},
    onRetry: () => {},
    onVisible: (keys: string[]) => {
      reports.push(keys);
    },
  });
  const app = mount(PipelinesList, { target: host, props });
  mounted.push({ app, host });
  const scrollEl = host.querySelector<HTMLElement>('[data-testid="pipelines-list"]')!;
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
  it('mounts the rows near the viewport of thousands, from the top', async () => {
    const { scrollEl } = mountPulls();
    await settle(scrollEl);
    await waitFor(() => onScreenKeys(scrollEl, 'data-pr-key').length > 0, 'rows on screen');

    const keys = mountedKeys(scrollEl, 'data-pr-key');
    expect(keys.length).toBeGreaterThan(0);
    expect(keys.length).toBeLessThan(MOUNTED_BOUND);
    expect(onScreenKeys(scrollEl, 'data-pr-key')[0]).toBe(pullRow(0, 'needs_you').key);

    // Scrolling to the end renders the last row.
    scrollEl.scrollTop = scrollEl.scrollHeight;
    await settle(scrollEl);
    const lastKey = pullRow(NEEDS_YOU + WAITING - 1, 'waiting').key;
    await waitFor(() => onScreenKeys(scrollEl, 'data-pr-key').includes(lastKey), 'the last row on screen');
    expect(mountedKeys(scrollEl, 'data-pr-key').length).toBeLessThan(MOUNTED_BOUND);
    expect(onScreenKeys(scrollEl, 'data-pr-key')).not.toContain(pullRow(0, 'needs_you').key);
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

  it('keeps its scroll position across being hidden and shown again', async () => {
    const { host, props, scrollEl } = mountPulls();
    await settle(scrollEl);
    scrollEl.scrollTop = 40_000;
    await settle(scrollEl);
    const before = onScreenKeys(scrollEl, 'data-pr-key');
    expect(before).not.toContain(pullRow(0, 'needs_you').key);
    const top = scrollEl.scrollTop;

    // The compact detail hides the list column with display: none.
    host.style.display = 'none';
    props.shown = false;
    flushSync();
    await raf();
    host.style.display = 'flex';
    props.shown = true;
    flushSync();
    await settle(scrollEl);
    expect(scrollEl.scrollTop).toBe(top);
    expect(onScreenKeys(scrollEl, 'data-pr-key')).toEqual(before);
  });

  it('opens an order the reader chose from the top, and follows the row at the top when a refresh re-sorts', async () => {
    const { props, scrollEl } = mountPulls();
    await settle(scrollEl);
    scrollEl.scrollTop = 40_000;
    await settle(scrollEl);
    const mid = onScreenKeys(scrollEl, 'data-pr-key');
    expect(mid[0]).not.toBe(pullRow(0, 'needs_you').key);

    // A refresh that re-sorts keeps the row that was at the top in view.
    const [needsYou, waiting] = props.groups;
    props.groups = [{ group: 'needs_you', rows: [...needsYou!.rows].reverse() }, waiting!];
    flushSync();
    await settle(scrollEl);
    expect(onScreenKeys(scrollEl, 'data-pr-key')[0]).toBe(mid[0]);
    expect(scrollEl.scrollTop).toBeGreaterThan(0);

    // The reader's own sort opens from the top.
    props.groups = [{ group: 'needs_you', rows: [...needsYou!.rows].reverse() }, { group: 'waiting', rows: [...waiting!.rows].reverse() }];
    props.orderKey = 'updated:reversed';
    flushSync();
    await settle(scrollEl);
    expect(scrollEl.scrollTop).toBe(0);
    expect(onScreenKeys(scrollEl, 'data-pr-key')[0]).toBe(pullRow(NEEDS_YOU - 1, 'needs_you').key);
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
    const { host, reports, scrollEl } = mountPipelines(
      Array.from({ length: 5_000 }, (_, index) => pipelineRow(index)),
      [{ projectId: 'p1', name: 'gadgets', message: 'The forge refused the list.', failure: null, forge: 'github' }],
    );
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

  it('reports no row while hidden, and the rows on screen again once shown', async () => {
    const { host, props, reports, scrollEl } = mountPipelines(Array.from({ length: 5_000 }, (_, index) => pipelineRow(index)));
    await settle(scrollEl);
    scrollEl.scrollTop = 40_000;
    await settle(scrollEl);
    await waitFor(() => lastReport(reports).length > 0 && !lastReport(reports).includes(pipelineRow(0).key), 'mid-list rows reported');
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
    expect(new Set(lastReport(reports))).toEqual(new Set(onScreenKeys(scrollEl, 'data-pipeline-key')));
  });

  it('reports no row once unmounted (the tab switches), and the rows on screen when mounted again', async () => {
    const rows = Array.from({ length: 5_000 }, (_, index) => pipelineRow(index));
    const first = mountPipelines(rows);
    await settle(first.scrollEl);
    await waitFor(() => lastReport(first.reports).length > 0, 'a visible report');
    const entry = mounted.splice(mounted.findIndex((candidate) => candidate.host === first.host), 1)[0]!;
    unmount(entry.app);
    entry.host.remove();
    expect(lastReport(first.reports)).toEqual([]);

    const again = mountPipelines(rows);
    await settle(again.scrollEl);
    await waitFor(() => lastReport(again.reports).length > 0, 'rows reported after the remount');
    expect(new Set(lastReport(again.reports))).toEqual(new Set(onScreenKeys(again.scrollEl, 'data-pipeline-key')));
  });

  it('stays at its top when the rows move under it there', async () => {
    const { props, reports, scrollEl } = mountPipelines(Array.from({ length: 5_000 }, (_, index) => pipelineRow(index)));
    await settle(scrollEl);
    await waitFor(() => lastReport(reports)[0] === pipelineRow(0).key, 'the first rows reported');
    expect(scrollEl.scrollTop).toBe(0);

    // The row at the top moves to the end; the list does not follow it.
    props.rows = [...props.rows.slice(1), props.rows[0]!];
    flushSync();
    await settle(scrollEl);
    expect(scrollEl.scrollTop).toBe(0);
    expect(lastReport(reports)[0]).toBe(pipelineRow(1).key);
    expect(new Set(lastReport(reports))).toEqual(new Set(onScreenKeys(scrollEl, 'data-pipeline-key')));
  });

  it('reports the rows on screen when the rows change under a still viewport', async () => {
    const { props, reports, scrollEl } = mountPipelines(Array.from({ length: 5_000 }, (_, index) => pipelineRow(index)));
    props.rows = Array.from({ length: 20 }, (_, index) => pipelineRow(index));
    flushSync();
    await settle(scrollEl);
    await waitFor(() => lastReport(reports)[0] === pipelineRow(0).key, 'the first rows reported');

    // The same rows re-sorted: every height is measured and unchanged, so
    // nothing but the data says which rows are on screen now.
    props.rows = [...props.rows].reverse();
    flushSync();
    await settle(scrollEl);
    expect(lastReport(reports)[0]).toBe(pipelineRow(19).key);
    expect(new Set(lastReport(reports))).toEqual(new Set(onScreenKeys(scrollEl, 'data-pipeline-key')));

    props.rows = [];
    flushSync();
    await settle(scrollEl);
    expect(lastReport(reports)).toEqual([]);
  });
});
