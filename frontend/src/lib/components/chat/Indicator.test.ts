// Verifies the Indicator contract: a text-free state dot whose
// presence and styling encode the row's run state. The `null` case is
// load-bearing — it MUST render nothing (success / idle is signalled
// by absence, not by a green badge). Stagger for the backgrounded
// variant is asserted on Tailwind arbitrary-value classes rather than
// inline style attributes so the test stays decoupled from how the
// animation delay is wired.

import { describe, expect, it } from 'vitest';
import { render } from '@testing-library/svelte';
import Indicator from './Indicator.svelte';

describe('<Indicator>', () => {
  it('renders nothing when state is null', () => {
    const { container } = render(Indicator, { props: { state: null } });
    expect(container.querySelector('[data-testid="indicator"]')).toBeNull();
  });

  it('renders nothing even when class is supplied with null state', () => {
    // A caller that unconditionally passes a class shouldn't leak any
    // dot DOM. Defends against accidentally rendering an empty
    // styled wrapper while still claiming "no indicator".
    const { container } = render(Indicator, { props: { state: null, class: 'ml-2' } });
    expect(container.querySelector('[data-testid="indicator"]')).toBeNull();
    expect(container.textContent?.trim() ?? '').toBe('');
  });

  it('renders a pulsing accent dot when running', () => {
    const { container } = render(Indicator, { props: { state: 'running' } });
    const dot = container.querySelector('[data-testid="indicator"]')!;
    expect(dot.getAttribute('data-state')).toBe('running');
    expect(dot.className).toContain('bg-accent');
    expect(dot.className).toContain('animate-pulse');
    expect(dot.getAttribute('aria-label')).toBe('Running');
  });

  it('renders three staggered accent dots when backgrounded', () => {
    const { container } = render(Indicator, { props: { state: 'backgrounded' } });
    const wrapper = container.querySelector('[data-testid="indicator"]')!;
    expect(wrapper.getAttribute('data-state')).toBe('backgrounded');
    expect(wrapper.getAttribute('aria-label')).toBe('Backgrounded');
    const dots = wrapper.querySelectorAll('span');
    expect(dots.length).toBe(3);
    for (const d of dots) {
      expect(d.className).toContain('animate-pulse');
      expect(d.className).toContain('bg-accent');
    }
    // Stagger is encoded via Tailwind arbitrary-value classes. Dot 1
    // has no delay class; dots 2 and 3 each carry one. Asserting on
    // the class (the behavior contract) instead of the inline style
    // keeps the test resilient to how the animation delay is wired.
    // The delays must stay multiples of the stepped animate-pulse jump
    // interval (250ms) so all three dots present on the same instants.
    expect(dots[0].className).not.toContain('animation-delay');
    expect(dots[1].className).toContain('[animation-delay:250ms]');
    expect(dots[2].className).toContain('[animation-delay:500ms]');
  });

  it('renders a static red dot when error', () => {
    const { container } = render(Indicator, { props: { state: 'error' } });
    const dot = container.querySelector('[data-testid="indicator"]')!;
    expect(dot.getAttribute('data-state')).toBe('error');
    expect(dot.className).toContain('bg-error');
    expect(dot.className).not.toContain('animate-pulse');
    expect(dot.getAttribute('aria-label')).toBe('Errored');
  });

  it('renders a static amber dot when declined', () => {
    const { container } = render(Indicator, { props: { state: 'declined' } });
    const dot = container.querySelector('[data-testid="indicator"]')!;
    expect(dot.getAttribute('data-state')).toBe('declined');
    expect(dot.className).toContain('bg-warning');
    expect(dot.className).not.toContain('animate-pulse');
    expect(dot.getAttribute('aria-label')).toBe('Declined');
  });

  it('honors an explicit ariaLabel override', () => {
    const { container } = render(Indicator, {
      props: { state: 'error', ariaLabel: 'Command failed with exit 137' },
    });
    const dot = container.querySelector('[data-testid="indicator"]')!;
    expect(dot.getAttribute('aria-label')).toBe('Command failed with exit 137');
  });

  it('merges caller-supplied class', () => {
    const { container } = render(Indicator, {
      props: { state: 'running', class: 'ml-2 custom-bonus' },
    });
    const dot = container.querySelector('[data-testid="indicator"]')!;
    expect(dot.className).toContain('ml-2');
    expect(dot.className).toContain('custom-bonus');
    expect(dot.className).toContain('bg-accent');
  });

  it('does not render literal "undefined" in className when class is omitted', () => {
    // Defensive guard: when an optional `class` prop falls through, a
    // missing default would interpolate as the string `"undefined"`
    // into the rendered class attribute. The component sets a `''`
    // default — pin it so a future refactor that drops the default
    // fails this test.
    const { container } = render(Indicator, { props: { state: 'running' } });
    const dot = container.querySelector('[data-testid="indicator"]')!;
    expect(dot.className).not.toContain('undefined');
  });
});
