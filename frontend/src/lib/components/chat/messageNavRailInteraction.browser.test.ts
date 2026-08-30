// The message-nav rail is an overlay beside selectable transcript prose. This
// needs the real MessageTimeline and Chromium hit-test geometry: happy-dom has
// no layout, while a component-only rail fixture cannot prove where production
// row text begins.
import { describe, expect, it } from 'vitest';
import '../../../app.css';
import { tick } from 'svelte';
import {
  NAV_RAIL_HIT_WIDTH_PX,
  TICK_FULL_WIDTH_PX,
  TICK_REST_WIDTH_PX,
} from './messageNavRail';
import { waitFor } from '../../../test/helpers/browserFrames';
import {
  mountTimeline,
  seedTimelineItems,
  setupTimelineHarness,
  type QuietBottomOptions,
  type SeedProse,
} from '../../../test/helpers/timelineBrowserHarness';

const QUIET_BOTTOM: QuietBottomOptions = {
  epsilonPx: 2,
  stableFrames: 12,
  frameBudget: 480,
};

const PROSE: SeedProse = {
  question: (i) => `Question ${i}: where should the reader jump in this thread?`,
  replyLead: (i) => `Reply ${i}: selectable transcript prose must start beyond the navigation rail hit target.`,
  replyList: '- preserve the first character\n- keep the rail reachable',
};

setupTimelineHarness();

function resolvedBackground(variable: string): string {
  const probe = document.createElement('div');
  probe.style.backgroundColor = `var(${variable})`;
  document.body.appendChild(probe);
  const color = getComputedStyle(probe).backgroundColor;
  probe.remove();
  return color;
}

function hasVisibleShadow(value: string): boolean {
  const colors = value.match(/rgba?\([^)]*\)/g) ?? [];
  return colors.some((color) => {
    const channels = color.match(/[\d.]+/g)?.map(Number) ?? [];
    return channels.length === 3 || (channels[3] ?? 0) > 0;
  });
}

describe('message navigation rail interaction geometry', () => {
  it('leaves a real gap between its hit target and selectable transcript text', async () => {
    const threadId = 'thread-nav-rail-selection';
    const { host } = await mountTimeline(
      threadId,
      seedTimelineItems(threadId, PROSE),
      QUIET_BOTTOM,
    );

    const strip = host.querySelector('[data-testid="nav-rail-strip"]');
    expect(strip, 'the seeded thread must render its message rail').not.toBeNull();

    const prose = [...host.querySelectorAll<HTMLElement>('[data-item-kind="assistant_text"] p')]
      .find((el) => el.getBoundingClientRect().height > 0);
    expect(prose, 'a selectable assistant line must be mounted').toBeDefined();

    const rail = host.querySelector('[data-testid="message-nav-rail"]')!;
    const gap = prose!.getBoundingClientRect().left - rail.getBoundingClientRect().right;
    expect(gap, 'the invisible rail button must not intercept the start of a line')
      .toBeGreaterThanOrEqual(8);
    expect(rail.getBoundingClientRect().width).toBe(NAV_RAIL_HIT_WIDTH_PX);

    const ticks = [...host.querySelectorAll<HTMLElement>('.nav-rail-tick')];
    const current = ticks.find((el) => el.dataset.current === 'true');
    const hoverTarget = ticks.find((el) => el.dataset.current === 'false');
    expect(current, 'viewport sync must identify one current message').toBeDefined();
    expect(hoverTarget, 'the fixture must expose a non-current hover target').toBeDefined();
    expect(getComputedStyle(current!).backgroundColor).toBe(resolvedBackground('--color-accent'));
    expect(getComputedStyle(hoverTarget!).backgroundColor).toBe(resolvedBackground('--color-border'));

    const stripRect = strip!.getBoundingClientRect();
    const targetRect = hoverTarget!.getBoundingClientRect();
    expect(stripRect.width, 'cold acquisition must match the resting tick').toBeCloseTo(
      TICK_REST_WIDTH_PX,
      1,
    );
    const expandedOnlyX = stripRect.left + TICK_FULL_WIDTH_PX - 1;
    const targetY = targetRect.top + targetRect.height / 2;
    expect(
      document.elementFromPoint(expandedOnlyX, targetY),
      'the future fisheye area must be inert before deliberate acquisition',
    ).not.toBe(strip);
    expect(
      document.elementFromPoint(stripRect.left + 1, targetY),
      'the resting tick column must acquire the rail',
    ).toBe(strip);

    const hover = new MouseEvent('mousemove', { bubbles: true });
    Object.defineProperty(hover, 'offsetY', {
      value: targetY - stripRect.top,
    });
    strip!.dispatchEvent(hover);
    await tick();

    expect(strip!.getBoundingClientRect().width, 'an acquired rail must retain its fisheye area')
      .toBe(TICK_FULL_WIDTH_PX);
    expect(document.elementFromPoint(expandedOnlyX, targetY)).toBe(strip);
    expect(hoverTarget!.dataset.hovered).toBe('true');
    expect(getComputedStyle(hoverTarget!).backgroundColor)
      .toBe(resolvedBackground('--color-border-strong'));
    expect(getComputedStyle(current!).backgroundColor, 'current color must win over hover state')
      .toBe(resolvedBackground('--color-accent'));
    const transitions = getComputedStyle(hoverTarget!).transitionProperty;
    expect(transitions).toContain('transform');
    expect(transitions).not.toContain('background');
    expect(transitions).not.toContain('opacity');

    // Custom themes replace the base semantic variables. The rail consumes
    // those roles directly, so a live palette edit must repaint every state
    // without a component render or a rail-specific theme key.
    const rootStyle = document.documentElement.style;
    const previousThemeValues = new Map([
      ['--border', rootStyle.getPropertyValue('--border')],
      ['--border-strong', rootStyle.getPropertyValue('--border-strong')],
      ['--accent', rootStyle.getPropertyValue('--accent')],
    ]);
    try {
      rootStyle.setProperty('--border', 'rgb(17 34 51)');
      rootStyle.setProperty('--border-strong', 'rgb(68 85 102)');
      rootStyle.setProperty('--accent', 'rgb(119 136 153)');
      expect(getComputedStyle(ticks.find((el) => (
        el.dataset.current === 'false' && el.dataset.hovered === 'false'
      ))!).backgroundColor).toBe('rgb(17, 34, 51)');
      expect(getComputedStyle(hoverTarget!).backgroundColor).toBe('rgb(68, 85, 102)');
      expect(getComputedStyle(current!).backgroundColor).toBe('rgb(119, 136, 153)');
    } finally {
      for (const [name, value] of previousThemeValues) {
        if (value) rootStyle.setProperty(name, value);
        else rootStyle.removeProperty(name);
      }
    }
  });

  it('renders an aligned bare chevron while preserving its 24px button target', async () => {
    const threadId = 'thread-nav-rail-arrow';
    const { host } = await mountTimeline(
      threadId,
      seedTimelineItems(threadId, PROSE),
      QUIET_BOTTOM,
    );

    host.style.height = '260px';
    await waitFor(() => {
      const button = host.querySelector<HTMLElement>('[data-testid="nav-rail-jump-first"]');
      return button !== null && getComputedStyle(button).visibility === 'visible';
    }, 'first-message chevron to become visible after rail clipping');
    const first = host.querySelector<HTMLElement>('[data-testid="nav-rail-jump-first"]')!;

    const style = getComputedStyle(first);
    expect(first.getBoundingClientRect().width).toBe(24);
    expect(first.getBoundingClientRect().height).toBe(24);
    expect(style.backgroundColor).toBe('rgba(0, 0, 0, 0)');
    expect(style.borderTopStyle).toBe('none');
    expect(hasVisibleShadow(style.boxShadow)).toBe(false);

    const restingTick = host.querySelector<HTMLElement>('.nav-rail-tick')!;
    const tickCenter = restingTick.getBoundingClientRect().left + TICK_REST_WIDTH_PX / 2;
    const buttonCenter = first.getBoundingClientRect().left + first.getBoundingClientRect().width / 2;
    expect(buttonCenter).toBeCloseTo(tickCenter, 1);
    const strip = host.querySelector<HTMLElement>('[data-testid="nav-rail-strip"]')!;
    expect(first.getBoundingClientRect().bottom).toBeLessThan(strip.getBoundingClientRect().top);
  });
});
