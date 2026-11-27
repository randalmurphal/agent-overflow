// Real-Chromium suite for a dashboard's line chart of a sprint's days (a
// burndown): drawn to its box's width, an even pace from its Start's
// value to zero on the sprint's last day, each point saying what is left
// and, on hover, what the pace would leave; a day still to come has no
// point. A chart of periods draws no pace.

import { afterEach, describe, expect, it, vi } from 'vitest';
import { flushSync, mount, unmount } from 'svelte';
import '../../../../app.css';
import type { BucketValue } from '../../../../../bindings/agent-overflow/internal/trackerdash/models';
import { waitFor } from '../../../../test/helpers/browserFrames';
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
