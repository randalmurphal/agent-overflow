import { describe, expect, it } from 'vitest';
import { render } from '@testing-library/svelte';
import type { ComponentProps } from 'svelte';

import MeterRing from './MeterRing.svelte';

const CIRCUMFERENCE = 2 * Math.PI * 9.75;

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
    expect(text!.getAttribute('x')).toBe('12');
    expect(text!.getAttribute('y')).toBe('12');
    expect(text!.getAttribute('text-anchor')).toBe('middle');
    expect(text!.getAttribute('dominant-baseline')).toBe('central');
  });

  // Label size is fixed in viewBox units so it scales with the ring
  // through the single viewBox transform — the same ratio the old HTML
  // label had (0.53125rem text in a 1.75rem ring).
  it('sizes the label in viewBox units at the legacy px ratio', () => {
    const { container } = renderRing();
    const text = container.querySelector('svg text')!;
    expect(Number(text.getAttribute('font-size'))).toBeCloseTo((0.53125 / 1.75) * 24, 10);
  });

  // The circles rotate -90° so the arc originates at 12 o'clock; the
  // label must sit outside that group or it renders sideways.
  it('rotates the circles but not the label', () => {
    const { container } = renderRing();
    const group = container.querySelector('svg g')!;
    expect(group.getAttribute('transform')).toBe('rotate(-90 12 12)');
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

  it('omits the arc when showArc is false, leaving only the track', () => {
    const { container } = renderRing({ showArc: false });
    expect(container.querySelectorAll('svg circle').length).toBe(1);
    // The label still renders on the empty ring.
    expect(container.querySelector('svg text')!.textContent).toBe('34');
  });
});
