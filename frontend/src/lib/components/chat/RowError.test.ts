// Verifies the RowError contract: a tone-styled sub-line that
// renders an optional short `code` chip plus a one-line `msg`.
// Body-column alignment is caller-owned (RowError stays
// geometry-agnostic) so these tests cover the tone palette, the
// optional chip, and the class merge — not positioning.

import { describe, expect, it } from 'vitest';
import { render } from '@testing-library/svelte';
import RowError from './RowError.svelte';

describe('<RowError>', () => {
  it('renders an error-toned sub-line with code chip and message', () => {
    const { container } = render(RowError, {
      props: { tone: 'error', code: 'exit 137', msg: 'Process killed' },
    });
    const root = container.querySelector('[data-testid="row-error"]')!;
    expect(root.getAttribute('data-tone')).toBe('error');

    const chip = container.querySelector('[data-testid="row-error-code"]')!;
    expect(chip).not.toBeNull();
    expect(chip.textContent).toBe('exit 137');
    expect(chip.className).toContain('bg-error/10');
    expect(chip.className).toContain('text-error');

    const msg = container.querySelector('[data-testid="row-error-msg"]')!;
    expect(msg.textContent).toBe('Process killed');
    expect(msg.className).toContain('text-error/90');
  });

  it('renders a declined-toned sub-line with the warning palette', () => {
    const { container } = render(RowError, {
      props: { tone: 'declined', msg: 'User declined approval' },
    });
    const root = container.querySelector('[data-testid="row-error"]')!;
    expect(root.getAttribute('data-tone')).toBe('declined');

    const msg = container.querySelector('[data-testid="row-error-msg"]')!;
    expect(msg.className).toContain('text-warning/90');
  });

  it('omits the code chip when code is not provided', () => {
    const { container } = render(RowError, {
      props: { tone: 'error', msg: 'Generic failure' },
    });
    expect(container.querySelector('[data-testid="row-error-code"]')).toBeNull();
    const msg = container.querySelector('[data-testid="row-error-msg"]')!;
    expect(msg.textContent).toBe('Generic failure');
  });

  it('renders an empty-string code as no chip (truthy guard)', () => {
    const { container } = render(RowError, {
      props: { tone: 'error', code: '', msg: 'No code' },
    });
    expect(container.querySelector('[data-testid="row-error-code"]')).toBeNull();
  });

  it('merges caller-supplied class onto the root', () => {
    const { container } = render(RowError, {
      props: { tone: 'error', msg: 'x', class: 'mt-1 custom-bonus' },
    });
    const root = container.querySelector('[data-testid="row-error"]')!;
    expect(root.className).toContain('mt-1');
    expect(root.className).toContain('custom-bonus');
    expect(root.className).toContain('flex');
  });

  it('does not render literal "undefined" in className', () => {
    // Defensive guard: when an optional `class` prop falls through, a
    // missing default would interpolate as the string `"undefined"`
    // into the rendered class attribute. The component sets a `''`
    // default — pin it so a future refactor that drops the default
    // fails this test.
    const { container } = render(RowError, {
      props: { tone: 'error', msg: 'x' },
    });
    const root = container.querySelector('[data-testid="row-error"]')!;
    expect(root.className).not.toContain('undefined');
  });
});
