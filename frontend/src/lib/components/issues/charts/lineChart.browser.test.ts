// Real-Chromium suite for a dashboard's line chart of a sprint's days (a
// burndown): drawn to its box's width, an even pace from its Start's
// value to zero on the sprint's last day, each point saying what is left
// and, on hover, what the pace would leave; a day still to come has no
// point. A chart of periods draws no pace. Both charts label their value
// axis in the unit their numbers read in. Bars by period outline the bar
// of a period still running.

import { afterEach, describe, expect, it, vi } from 'vitest';
import { flushSync, mount, unmount } from 'svelte';
import '../../../../app.css';
import type { BucketValue } from '../../../../../bindings/agent-overflow/internal/trackerdash/models';
import { waitFor } from '../../../../test/helpers/browserFrames';
import BarChart from './BarChart.svelte';
import LineChart from './LineChart.svelte';

const mounted: { app: object; host: HTMLElement }[] = [];

afterEach(() => {
  for (const { app, host } of mounted.splice(0)) {
    unmount(app);
    host.remove();
  }
});

function day(key: string, label: string, v: object, over: object = {}): BucketValue {
  return { key, label, cells: [v], total: v, ...over } as unknown as BucketValue;
}

const BUCKETS = [
  day('day:start', 'Start', { v: 12 }),
  day('day:2026-11-23', 'Nov 23', { v: 10, nc: 1 }),
  day('day:2026-11-24', 'Nov 24', { v: 7 }, { partial: true }),
  day('day:2026-11-25', 'Nov 25', {}, { last: true }),
];

async function draw(buckets: BucketValue[], pace: boolean) {
  const host = document.createElement('div');
  host.style.width = '480px';
  document.body.append(host);
  const onPick = vi.fn();
  const app = mount(LineChart, { target: host, props: { title: 'Burndown', buckets, series: [''], unit: 'estimate', estimates: 'points', onPick, pace } });
  mounted.push({ app, host });
  flushSync();
  await waitFor(() => host.querySelectorAll('[data-testid="dashboard-point"]').length === buckets.length, 'the chart drew its points');
  return { host, onPick };
}

describe('a burndown', () => {
  it('draws its even pace to zero on the last day, and each point says what is left beside the pace', async () => {
    const { host, onPick } = await draw(BUCKETS, true);
    const points = [...host.querySelectorAll<SVGGElement>('[data-testid="dashboard-point"]')];
    expect(points.map((p) => p.getAttribute('aria-label'))).toEqual([
      'Start, 12 points left',
      'Nov 23, 10 points left, 1 not counted',
      'Nov 24, 7 points left (so far)',
      'Nov 25, None',
    ]);
    // A day still to come has no marker.
    expect(points.map((p) => p.querySelector('circle') !== null)).toEqual([true, true, true, false]);
    const pace = host.querySelector<SVGLineElement>('[data-testid="dashboard-pace"]')!;
    const at = (p: SVGGElement) => Number(p.querySelector('rect')!.getAttribute('x')) + 12;
    expect(Number(pace.getAttribute('x1'))).toBeCloseTo(at(points[0]));
    expect(Number(pace.getAttribute('x2'))).toBeCloseTo(at(points[3]));
    const startY = Number(points[0].querySelector('circle')!.getAttribute('cy'));
    expect(Number(pace.getAttribute('y1'))).toBeCloseTo(startY);
    expect(host.querySelector('[data-testid="dashboard-legend"]')?.textContent).toContain('On pace');
    points[1].dispatchEvent(new FocusEvent('focus'));
    flushSync();
    expect(host.querySelector('[data-testid="dashboard-hover-pace"]')?.textContent).toBe('On pace would be 8 points');
    points[3].dispatchEvent(new FocusEvent('focus'));
    flushSync();
    expect(host.querySelector('[data-testid="dashboard-hover-pace"]')?.textContent).toBe('On pace would be 0 points');
    points[2].dispatchEvent(new MouseEvent('click', { bubbles: true }));
    expect(onPick).toHaveBeenCalledWith(BUCKETS[2], '');
  });

  it('draws no pace when its Start has no value, and a chart of periods none at all', async () => {
    const unknown = await draw([day('day:start', 'Start', { nc: 2 }), ...BUCKETS.slice(1)], true);
    expect(unknown.host.querySelector('[data-testid="dashboard-pace"]')).toBeNull();
    const periods = await draw(BUCKETS, false);
    expect(periods.host.querySelector('[data-testid="dashboard-pace"]')).toBeNull();
    expect([...periods.host.querySelectorAll('[data-testid="dashboard-point"]')].map((p) => p.getAttribute('aria-label'))[0]).toBe('Start, 12 points');
  });
});

describe('the value axis', () => {
  const HOURS = [day('week:2026-11-16', 'Nov 16', { v: 3600 }), day('week:2026-11-23', 'Nov 23', { v: 7200 })];

  async function ticks(chart: typeof LineChart | typeof BarChart): Promise<string[]> {
    const host = document.createElement('div');
    host.style.width = '480px';
    document.body.append(host);
    const props = { title: 'Your hours', buckets: HOURS, series: [''], unit: 'time' as const, estimates: 'points', onPick: vi.fn() };
    const app = mount(chart as typeof LineChart, { target: host, props: chart === BarChart ? { ...props, horizontal: false } : props });
    mounted.push({ app, host });
    flushSync();
    await waitFor(() => host.querySelectorAll('[data-testid="dashboard-axis-tick"]').length > 0, 'the chart drew its axis');
    return [...host.querySelectorAll('[data-testid="dashboard-axis-tick"]')].map((t) => t.textContent ?? '');
  }

  it('reads logged time in hours on a line chart and on bars', async () => {
    expect(await ticks(LineChart)).toEqual(['0m', '30m', '1h', '1.5h', '2h']);
    expect(await ticks(BarChart)).toEqual(['0m', '30m', '1h', '1.5h', '2h']);
  });
});

describe('bars by period', () => {
  it('outline the bar of a period still running and fill it lighter, and no other bar', async () => {
    const host = document.createElement('div');
    host.style.width = '480px';
    document.body.append(host);
    const buckets = [day('sprint:1', 'Sprint 1', { v: 5 }), day('sprint:2', 'Sprint 2', { v: 3 }, { partial: true })];
    const app = mount(BarChart, { target: host, props: { title: 'Finished per sprint', buckets, series: [''], unit: 'estimate', estimates: 'points', onPick: vi.fn(), horizontal: false } });
    mounted.push({ app, host });
    flushSync();
    await waitFor(() => host.querySelectorAll('[data-testid="dashboard-bar"] path').length === 2, 'the chart drew its bars');
    const bars = [...host.querySelectorAll<SVGPathElement>('[data-testid="dashboard-bar"] path')];
    expect(bars.map((p) => [p.getAttribute('fill-opacity'), p.getAttribute('stroke')])).toEqual([
      ['1', 'none'],
      ['0.4', 'var(--chart-series-1)'],
    ]);
    // The outline is the series color at full strength; the fill is the same color.
    const stroke = getComputedStyle(bars[1]).stroke;
    expect(stroke).toBe(getComputedStyle(bars[0]).fill);
    expect(getComputedStyle(bars[1]).fill).toBe(stroke);
    expect(bars.map((p) => p.closest('[data-testid="dashboard-bar"]')?.getAttribute('aria-label'))).toEqual(['Sprint 1, 5 points', 'Sprint 2, 3 points (so far)']);
  });
});

describe('a value below 0', () => {
  function bars(title: string, horizontal: boolean): HTMLElement {
    const host = document.createElement('div');
    host.style.width = '480px';
    document.body.append(host);
    const app = mount(BarChart, { target: host, props: { title, buckets: NET, series: [''], unit: 'estimate', estimates: 'points', onPick: vi.fn(), horizontal } });
    mounted.push({ app, host });
    flushSync();
    return host;
  }
  const NET = [day('week:1', 'Week 1', { v: 6 }), day('week:2', 'Week 2', { v: -4 }), day('week:3', 'Week 3', { v: 0 })];

  it('is a bar under 0\'s line beside the bars above it, and a bar of 0 is none', async () => {
    const host = bars('Net change', false);
    await waitFor(() => host.querySelectorAll('[data-testid="dashboard-bar"] path').length === 2, 'the chart drew a bar for each value that is not 0');
    const ticks = [...host.querySelectorAll('[data-testid="dashboard-axis-tick"]')];
    expect(ticks.map((t) => t.textContent)).toEqual(['-5', '0', '5', '10']);
    const zero = ticks[1].getBoundingClientRect();
    const zeroY = zero.top + zero.height / 2;
    const [up, down] = [...host.querySelectorAll<SVGPathElement>('[data-testid="dashboard-bar"] path')].map((p) => p.getBoundingClientRect());
    // The bar of 6 stands on 0's line; the bar of -4 hangs from it, two thirds as long.
    expect(Math.abs(up.bottom - zeroY)).toBeLessThan(3);
    expect(Math.abs(down.top - zeroY)).toBeLessThan(3);
    expect(up.height).toBeGreaterThan(20);
    expect(down.height / up.height).toBeCloseTo(4 / 6, 1);
    expect(host.querySelectorAll('[data-testid="dashboard-bar"] path[data-below]')).toHaveLength(1);
    expect([...host.querySelectorAll('[data-testid="dashboard-bar"]')].map((b) => b.getAttribute('aria-label'))).toEqual(['Week 1, 6 points', 'Week 2, -4 points', 'Week 3, 0 points']);
    // Every bar, the one below 0 included, is inside the chart's box.
    const box = host.querySelector('svg')!.getBoundingClientRect();
    expect(down.bottom).toBeLessThanOrEqual(box.bottom);
  });

  it('is a bar left of 0\'s line on bars that lie flat', async () => {
    const host = bars('Net change by person', true);
    await waitFor(() => host.querySelectorAll('[data-testid="dashboard-bar"] path').length === 2, 'the chart drew a bar for each value that is not 0');
    const lines = [...host.querySelectorAll<SVGLineElement>('[data-testid="dashboard-bar-zero"]')].map((l) => l.getBoundingClientRect());
    expect(lines).toHaveLength(3);
    const [right, left] = [...host.querySelectorAll<SVGPathElement>('[data-testid="dashboard-bar"] path')].map((p) => p.getBoundingClientRect());
    expect(Math.abs(right.left - lines[0].left)).toBeLessThan(3);
    expect(Math.abs(left.right - lines[1].left)).toBeLessThan(3);
    expect(left.width / right.width).toBeCloseTo(4 / 6, 1);
    const plot = host.querySelector('svg')!.getBoundingClientRect();
    expect(left.left).toBeGreaterThanOrEqual(plot.left - 1);
  });

  it('is a point under 0\'s line on a line chart, inside the chart', async () => {
    const host = document.createElement('div');
    host.style.width = '480px';
    document.body.append(host);
    const app = mount(LineChart, { target: host, props: { title: 'Net change', buckets: NET, series: [''], unit: 'estimate', estimates: 'points', onPick: vi.fn() } });
    mounted.push({ app, host });
    flushSync();
    await waitFor(() => host.querySelectorAll('[data-testid="dashboard-point"] circle').length === 3, 'the chart drew its points');
    const ticks = [...host.querySelectorAll('[data-testid="dashboard-axis-tick"]')];
    expect(ticks.map((t) => t.textContent)).toEqual(['-5', '0', '5', '10']);
    const zero = ticks[1].getBoundingClientRect();
    const zeroY = zero.top + zero.height / 2;
    const centers = [...host.querySelectorAll('[data-testid="dashboard-point"] circle')].map((c) => {
      const r = c.getBoundingClientRect();
      return r.top + r.height / 2;
    });
    expect(centers[0]).toBeLessThan(zeroY - 10);
    expect(centers[1]).toBeGreaterThan(zeroY + 10);
    expect(Math.abs(centers[2] - zeroY)).toBeLessThan(3);
    const box = host.querySelector('svg')!.getBoundingClientRect();
    expect(centers[1]).toBeLessThan(box.bottom - 20);
  });
});
