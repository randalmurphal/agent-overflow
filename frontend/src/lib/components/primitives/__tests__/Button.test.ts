// Verifies the Button primitive's core contract:
//   - renders a native <button> with the correct type + visual classes
//   - variant/size props drive the class composition
//   - leading/trailing/loading snippets render in the right order
//   - disabled + loading both block click AND set the native disabled attr
//   - accessibility attributes (aria-label, aria-busy) flow through
//   - onclick fires on enabled button, not on disabled/loading

import { describe, expect, it, vi } from 'vitest';
import { render, fireEvent } from '@testing-library/svelte';
import Harness from './ButtonHarness.svelte';

describe('<Button>', () => {
  it('renders a button with the default label', () => {
    const { getByRole } = render(Harness, { props: { label: 'Save' } });
    const btn = getByRole('button');
    expect(btn.textContent).toContain('Save');
    expect(btn.getAttribute('type')).toBe('button');
  });

  it('defaults to variant=secondary and size=sm', () => {
    const { getByRole } = render(Harness);
    const cls = getByRole('button').className;
    // secondary variant signature
    expect(cls).toContain('border-border-subtle');
    // sm size signature
    expect(cls).toContain('h-7');
    expect(cls).toContain('text-xs');
  });

  it('applies primary variant classes', () => {
    const { getByRole } = render(Harness, { props: { variant: 'primary' } });
    const cls = getByRole('button').className;
    expect(cls).toContain('bg-accent');
    expect(cls).toContain('text-surface-0');
  });

  it('applies ghost variant classes', () => {
    const { getByRole } = render(Harness, { props: { variant: 'ghost' } });
    const cls = getByRole('button').className;
    expect(cls).toContain('bg-transparent');
    expect(cls).toContain('text-fg-muted');
  });

  it('applies danger variant classes', () => {
    const { getByRole } = render(Harness, { props: { variant: 'danger' } });
    const cls = getByRole('button').className;
    expect(cls).toContain('bg-error');
  });

  it('applies danger-outline variant (outlined error, not filled)', () => {
    const { getByRole } = render(Harness, { props: { variant: 'danger-outline' } });
    const cls = getByRole('button').className;
    expect(cls).toContain('border-error/40');
    expect(cls).toContain('text-error/90');
    expect(cls).not.toMatch(/(?<!\/)bg-error(?![-/])/); // not filled
  });

  it('applies danger-ghost variant (error text, transparent rest)', () => {
    const { getByRole } = render(Harness, { props: { variant: 'danger-ghost' } });
    const cls = getByRole('button').className;
    expect(cls).toContain('text-error/80');
    expect(cls).toContain('bg-transparent');
    expect(cls).not.toContain('border-error');
  });

  it('applies tinted variant (soft-accent fill)', () => {
    const { getByRole } = render(Harness, { props: { variant: 'tinted' } });
    const cls = getByRole('button').className;
    expect(cls).toContain('bg-accent/15');
    expect(cls).toContain('text-accent');
  });

  it('applies xs size', () => {
    const { getByRole } = render(Harness, { props: { size: 'xs' } });
    const cls = getByRole('button').className;
    expect(cls).toContain('h-6');
    expect(cls).toContain('text-[11px]');
  });

  it('applies md size', () => {
    const { getByRole } = render(Harness, { props: { size: 'md' } });
    const cls = getByRole('button').className;
    expect(cls).toContain('h-8');
    expect(cls).toContain('text-sm');
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
