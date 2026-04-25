// Verifies the CompletionBadge contract:
//   - exposes data-testid + data-status for selectors
//   - swaps icon + palette per status
//   - applies theme tokens (success/error), not literal emerald/rose
//   - accepts title override; falls back to a sensible aria-label

import { describe, expect, it } from 'vitest';
import { render } from '@testing-library/svelte';
import CompletionBadge from './CompletionBadge.svelte';

describe('<CompletionBadge>', () => {
  it('renders the success variant with the success palette and a check icon', () => {
    const { container } = render(CompletionBadge, { props: { status: 'success' } });
    const badge = container.querySelector('[data-testid="completion-badge"]')!;
    expect(badge).not.toBeNull();
    expect(badge.getAttribute('data-status')).toBe('success');
    expect(badge.className).toContain('bg-success/10');
    expect(badge.className).toContain('text-success');
    expect(container.querySelector('svg.lucide-check')).not.toBeNull();
  });

  it('renders the failure variant with the error palette and an alert icon', () => {
    const { container } = render(CompletionBadge, { props: { status: 'failure' } });
    const badge = container.querySelector('[data-testid="completion-badge"]')!;
    expect(badge.getAttribute('data-status')).toBe('failure');
    expect(badge.className).toContain('bg-error/10');
    expect(badge.className).toContain('text-error');
    // lucide-svelte 1.0.1 aliases alert-circle → circle-alert.
    expect(container.querySelector('svg.lucide-circle-alert')).not.toBeNull();
  });

  it('uses a default aria-label per status when no title is supplied', () => {
    const { container, rerender } = render(CompletionBadge, { props: { status: 'success' } });
    let badge = container.querySelector('[data-testid="completion-badge"]')!;
    expect(badge.getAttribute('aria-label')).toBe('Completed successfully');

    rerender({ status: 'failure' });
    badge = container.querySelector('[data-testid="completion-badge"]')!;
    expect(badge.getAttribute('aria-label')).toBe('Failed');
  });

  it('honors an explicit title prop for both tooltip and aria-label', () => {
    const { container } = render(CompletionBadge, {
      props: { status: 'failure', title: 'Approval declined' },
    });
    const badge = container.querySelector('[data-testid="completion-badge"]')!;
    expect(badge.getAttribute('title')).toBe('Approval declined');
    expect(badge.getAttribute('aria-label')).toBe('Approval declined');
  });

  it('merges caller-supplied class onto the defaults', () => {
    const { container } = render(CompletionBadge, {
      props: { status: 'success', class: 'ml-auto custom-bonus' },
    });
    const badge = container.querySelector('[data-testid="completion-badge"]')!;
    expect(badge.className).toContain('rounded');
    expect(badge.className).toContain('ml-auto');
    expect(badge.className).toContain('custom-bonus');
  });

  it('does not render literal "undefined" in className when class prop is omitted', () => {
    // Defensive guard: when an optional `class` prop falls through, a
    // missing default would interpolate as the string `"undefined"`
    // into the rendered class attribute. The component sets a `''`
    // default — pin it so a future refactor that drops the default
    // fails this test.
    const { container } = render(CompletionBadge, { props: { status: 'success' } });
    const badge = container.querySelector('[data-testid="completion-badge"]')!;
    expect(badge.className).not.toContain('undefined');
  });
});
