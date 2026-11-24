// Verifies the Button primitive's core contract:
//   - renders a native <button> with the correct type + visual classes
//   - variant/size props drive the class composition
//   - leading/trailing/loading snippets render in the right order
//   - disabled + loading both block click AND set the native disabled attr
//   - accessibility attributes (aria-label, aria-busy) flow through
//   - onclick fires on enabled button, not on disabled/loading

import { describe, expect, it, vi } from 'vitest';
import { render, fireEvent } from '@testing-library/svelte';
import type { ComponentProps } from 'svelte';
import Harness from './ButtonHarness.svelte';

describe('<Button>', () => {
  it('renders a button with the default label', () => {
    const { getByRole } = render(Harness, { props: { label: 'Save' } });
    const btn = getByRole('button');
    expect(btn.textContent).toContain('Save');
    expect(btn.getAttribute('type')).toBe('button');
  });

  // `lacks` uses toMatch so a pattern can say "not filled" without also
  // rejecting the hover and opacity forms of the same token.
  it.each<{ name: string; props: ComponentProps<typeof Harness>; has: string[]; lacks?: (string | RegExp)[] }>([
    { name: 'the defaults, variant=secondary and size=sm', props: {}, has: ['border-border-subtle', 'h-7', 'text-xs'] },
    { name: 'primary', props: { variant: 'primary' }, has: ['bg-accent', 'text-surface-0'] },
    { name: 'ghost', props: { variant: 'ghost' }, has: ['bg-transparent', 'text-fg-muted'] },
    { name: 'danger', props: { variant: 'danger' }, has: ['bg-error'] },
    {
      name: 'danger-outline (outlined error, not filled)',
      props: { variant: 'danger-outline' },
      has: ['border-error/40', 'text-error/90'],
      lacks: [/(?<!\/)bg-error(?![-/])/],
    },
    {
      name: 'warning-outline (outlined warning, not filled)',
      props: { variant: 'warning-outline' },
      has: ['border-warning/50', 'text-warning'],
      lacks: [/(?<![:/])bg-warning(?![-/])/],
    },
    {
      name: 'danger-ghost (error text, transparent rest)',
      props: { variant: 'danger-ghost' },
      has: ['text-error/80', 'bg-transparent'],
      lacks: ['border-error'],
    },
    { name: 'tinted (soft-accent fill)', props: { variant: 'tinted' }, has: ['bg-accent/15', 'text-accent'] },
    { name: 'xs size', props: { size: 'xs' }, has: ['h-6', 'text-[0.6875rem]'] },
    { name: 'md size', props: { size: 'md' }, has: ['h-8', 'text-sm'] },
  ])('applies the classes of $name', ({ props, has, lacks = [] }) => {
    const { getByRole } = render(Harness, { props });
    const cls = getByRole('button').className;
    for (const token of has) expect(cls).toContain(token);
    for (const token of lacks) expect(cls).not.toMatch(token);
  });

  it('propagates type="submit"', () => {
    const { getByRole } = render(Harness, { props: { type: 'submit' } });
    expect(getByRole('button').getAttribute('type')).toBe('submit');
  });

  it('propagates title and aria-label', () => {
    const { getByRole } = render(Harness, {
      props: { title: 'tip', ariaLabel: 'a11y label' },
    });
    const btn = getByRole('button');
    expect(btn.getAttribute('title')).toBe('tip');
    expect(btn.getAttribute('aria-label')).toBe('a11y label');
  });

  it('fires onclick when enabled', async () => {
    const onclick = vi.fn();
    const { getByRole } = render(Harness, { props: { onclick } });
    await fireEvent.click(getByRole('button'));
    expect(onclick).toHaveBeenCalledTimes(1);
  });

  it('does not fire onclick when disabled', async () => {
    const onclick = vi.fn();
    const { getByRole } = render(Harness, { props: { disabled: true, onclick } });
    const btn = getByRole('button') as HTMLButtonElement;
    expect(btn.disabled).toBe(true);
    await fireEvent.click(btn);
    expect(onclick).not.toHaveBeenCalled();
  });

  it('does not fire onclick when loading and shows aria-busy', async () => {
    const onclick = vi.fn();
    const { getByRole } = render(Harness, { props: { loading: true, onclick } });
    const btn = getByRole('button') as HTMLButtonElement;
    expect(btn.getAttribute('aria-busy')).toBe('true');
    expect(btn.disabled).toBe(true);
    await fireEvent.click(btn);
    expect(onclick).not.toHaveBeenCalled();
  });

  it('shows the loading spinner (hides leading snippet) when loading=true', () => {
    const { queryByTestId, container } = render(Harness, {
      props: { loading: true, withLeading: true },
    });
    // Leading slot is suppressed during loading.
    expect(queryByTestId('leading')).toBeNull();
    // Spinner is a small animated span with animate-spin class.
    const spinner = container.querySelector('.animate-spin');
    expect(spinner).not.toBeNull();
  });

  it('renders leading slot when provided and not loading', () => {
    const { getByTestId } = render(Harness, { props: { withLeading: true } });
    expect(getByTestId('leading')).toBeInTheDocument();
  });

  it('renders trailing slot when provided and not loading', () => {
    const { getByTestId } = render(Harness, { props: { withTrailing: true } });
    expect(getByTestId('trailing')).toBeInTheDocument();
  });

  it('suppresses trailing slot while loading', () => {
    const { queryByTestId } = render(Harness, {
      props: { loading: true, withTrailing: true },
    });
    expect(queryByTestId('trailing')).toBeNull();
  });

  // Pressed prop drives both the aria-pressed attribute (for AT
  // consumers) AND a shared "depressed" visual so every variant can
  // double as a toggle. When pressed is undefined the attribute is
  // omitted (don't pretend a regular button is a toggle).
  it('omits aria-pressed when pressed is undefined', () => {
    const { getByRole } = render(Harness);
    expect(getByRole('button').hasAttribute('aria-pressed')).toBe(false);
  });

  it('sets aria-pressed="false" when pressed=false', () => {
    const { getByRole } = render(Harness, { props: { pressed: false } });
    expect(getByRole('button').getAttribute('aria-pressed')).toBe('false');
  });

  it('sets aria-pressed="true" and swaps to the pressed visual when pressed=true', () => {
    const { getByRole } = render(Harness, { props: { pressed: true, variant: 'ghost' } });
    const btn = getByRole('button');
    expect(btn.getAttribute('aria-pressed')).toBe('true');
    const cls = btn.className;
    // Pressed visual overrides the ghost rest palette.
    expect(cls).toContain('bg-surface-2/60');
    expect(cls).toContain('ring-inset');
    // The ghost rest hover classes must NOT apply while pressed (the
    // pressed classes win).
    expect(cls).not.toContain('hover:bg-surface-2/40');
  });
});
