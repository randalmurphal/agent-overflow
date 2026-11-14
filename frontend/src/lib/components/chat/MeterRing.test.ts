import { describe, expect, it } from 'vitest';
import { render } from '@testing-library/svelte';
import type { ComponentProps } from 'svelte';

import MeterRing from './MeterRing.svelte';

const CIRCUMFERENCE = 2 * Math.PI * 11.375;

function renderRing(props: Partial<ComponentProps<typeof MeterRing>> = {}) {
  return render(MeterRing, {
    props: { label: '34', percentage: 34, strokeClass: 'stroke-fg-subtle', ...props },
  });
}

describe('<MeterRing>', () => {
  // The label must be SVG text sharing the ring's viewBox coordinate
  // system, anchored on the ring center. An HTML span centered with
  // CSS drifts off-center at non-default zoom because Blink rounds
  // line-box baselines to whole pixels while SVG ink stays fractional.
  it('centers the label as SVG text on the ring center', () => {
    const { container } = renderRing();
    const text = container.querySelector('svg text');
    expect(text).toBeTruthy();
    expect(text!.textContent).toBe('34');
    expect(text!.getAttribute('x')).toBe('14');
    expect(text!.getAttribute('y')).toBe('14');
    expect(text!.getAttribute('text-anchor')).toBe('middle');
    expect(text!.getAttribute('dominant-baseline')).toBe('central');
  });

  // The viewBox must equal the h-7 rendered box at default zoom (28px):
  // a mismatched svg is a scaled replaced-content node whose transform
  // cache Blink re-allocates on every dashoffset tick (see the comment
  // in MeterRing.svelte).
  it('keeps the viewBox at the rendered 28px box so the svg is unscaled at default zoom', () => {
    const { container } = renderRing();
    const svg = container.querySelector('svg')!;
    expect(svg.getAttribute('viewBox')).toBe('0 0 28 28');
    expect(svg.getAttribute('class')).toContain('h-7');
    expect(svg.getAttribute('class')).toContain('w-7');
  });

  // Label size is fixed in viewBox units so it scales with the ring
  // through the single viewBox transform — the same ratio the old HTML
  // label had (0.53125rem = 8.5px text in a 1.75rem = 28px ring, and
  // the 28-unit viewBox is 1:1 with px at default zoom).
  it('sizes the label in viewBox units at the legacy px ratio', () => {
    const { container } = renderRing();
    const text = container.querySelector('svg text')!;
    expect(Number(text.getAttribute('font-size'))).toBe(8.5);
  });

  // The circles rotate -90° so the arc originates at 12 o'clock; the
  // label must sit outside that group or it renders sideways.
  it('rotates the circles but not the label', () => {
    const { container } = renderRing();
    const group = container.querySelector('svg g')!;
    expect(group.getAttribute('transform')).toBe('rotate(-90 14 14)');
    expect(group.querySelectorAll('circle').length).toBe(2);
    expect(group.querySelector('text')).toBeNull();
  });

  it('maps percentage to dashoffset (50% fills half the circumference)', () => {
    const { container } = renderRing({ percentage: 50 });
    const arc = container.querySelectorAll('svg circle')[1];
    expect(Number(arc.getAttribute('stroke-dashoffset'))).toBeCloseTo(CIRCUMFERENCE / 2, 6);
  });

  it('clamps out-of-range percentages so the arc stays within one revolution', () => {
    const over = renderRing({ percentage: 150 });
    const overArc = over.container.querySelectorAll('svg circle')[1];
    expect(Number(overArc.getAttribute('stroke-dashoffset'))).toBe(0);

    const under = renderRing({ percentage: -20 });
    const underArc = under.container.querySelectorAll('svg circle')[1];
    expect(Number(underArc.getAttribute('stroke-dashoffset'))).toBeCloseTo(CIRCUMFERENCE, 6);
  });

  // NaN and Infinity are separate cases on purpose: a regression
  // narrowing the guard to !Number.isNaN would pass the NaN test but
  // leak Infinity into the dash math.
  it('treats a NaN percentage as 0 so a wire glitch cannot break the dash math', () => {
    const { container } = renderRing({ percentage: NaN });
    const arc = container.querySelectorAll('svg circle')[1];
    expect(Number(arc.getAttribute('stroke-dashoffset'))).toBeCloseTo(CIRCUMFERENCE, 6);
  });

  it('treats an Infinity percentage as 0', () => {
    const { container } = renderRing({ percentage: Infinity });
    const arc = container.querySelectorAll('svg circle')[1];
    expect(Number(arc.getAttribute('stroke-dashoffset'))).toBeCloseTo(CIRCUMFERENCE, 6);
  });

  it('draws a single ring at the full stroke width without an inner limit', () => {
    const { container } = renderRing();
    const circles = [...container.querySelectorAll('svg circle')];
    expect(circles.map((c) => c.getAttribute('r'))).toEqual(['11.375', '11.375']);
    expect(circles.map((c) => c.getAttribute('stroke-width'))).toEqual(['3.5', '3.5']);
    expect(container.querySelector('[data-meter-arc="inner"]')).toBeNull();
  });

  // With an inner limit both rings thin to fit the same 28-unit box: the
  // main limit outside (outer edge 13 of 14), the inner one a unit inside it.
  it('draws two thin concentric rings when an inner limit is given', () => {
    const { container } = renderRing({
      percentage: 50,
      inner: { percentage: 25, strokeClass: 'stroke-error' },
    });
    const circles = [...container.querySelectorAll('svg circle')];
    expect(circles.map((c) => c.getAttribute('r'))).toEqual(['12', '12', '9', '9']);
    expect(circles.map((c) => c.getAttribute('stroke-width'))).toEqual(['2', '2', '2', '2']);

    const main = container.querySelector('[data-meter-arc="main"]')!;
    expect(main.getAttribute('class')).toContain('stroke-fg-subtle');
    expect(Number(main.getAttribute('stroke-dashoffset'))).toBeCloseTo(Math.PI * 12, 6);

    const inner = container.querySelector('[data-meter-arc="inner"]')!;
    expect(inner.getAttribute('class')).toContain('stroke-error');
    expect(Number(inner.getAttribute('stroke-dasharray'))).toBeCloseTo(2 * Math.PI * 9, 6);
    expect(Number(inner.getAttribute('stroke-dashoffset'))).toBeCloseTo(2 * Math.PI * 9 * 0.75, 6);
    // The viewBox and label are unchanged, so the meter keeps its footprint.
    expect(container.querySelector('svg')!.getAttribute('viewBox')).toBe('0 0 28 28');
    expect(container.querySelector('svg text')!.textContent).toBe('34');
  });

  it('guards the inner percentage against out-of-range and non-finite values', () => {
    const over = renderRing({ inner: { percentage: 150, strokeClass: 'stroke-error' } });
    expect(Number(over.container.querySelector('[data-meter-arc="inner"]')!.getAttribute('stroke-dashoffset'))).toBe(0);
    const nan = renderRing({ inner: { percentage: NaN, strokeClass: 'stroke-error' } });
    expect(Number(nan.container.querySelector('[data-meter-arc="inner"]')!.getAttribute('stroke-dashoffset')))
      .toBeCloseTo(2 * Math.PI * 9, 6);
  });

  it('returns to the single ring when the inner limit goes away', async () => {
    const { container, rerender } = renderRing({ inner: { percentage: 25, strokeClass: 'stroke-error' } });
    expect(container.querySelectorAll('svg circle').length).toBe(4);
    await rerender({ inner: undefined });
    const circles = [...container.querySelectorAll('svg circle')];
    expect(circles.map((c) => c.getAttribute('r'))).toEqual(['11.375', '11.375']);
    expect(circles.map((c) => c.getAttribute('stroke-width'))).toEqual(['3.5', '3.5']);
  });

  it('omits the arc when showArc is false, leaving only the track', () => {
    const { container } = renderRing({ showArc: false });
    expect(container.querySelectorAll('svg circle').length).toBe(1);
    // The label still renders on the empty ring.
    expect(container.querySelector('svg text')!.textContent).toBe('34');
  });
});
