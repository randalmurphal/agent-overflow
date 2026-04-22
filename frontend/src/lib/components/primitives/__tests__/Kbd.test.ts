// Verifies the Kbd primitive's contract:
//   - renders a <kbd> element with children content
//   - default classes provide the mono/uppercase/bordered pill look
//   - caller-supplied class is appended, not replaced

import { describe, expect, it } from 'vitest';
import { render } from '@testing-library/svelte';
import Harness from './KbdHarness.svelte';

describe('<Kbd>', () => {
  it('renders a <kbd> with its content', () => {
    const { container } = render(Harness, { props: { label: '⌘K' } });
    const kbd = container.querySelector('kbd')!;
    expect(kbd).not.toBeNull();
    expect(kbd.textContent).toContain('⌘K');
  });

  it('applies the baseline mono + uppercase + bordered pill classes', () => {
    const { container } = render(Harness);
    const kbd = container.querySelector('kbd')!;
    const cls = kbd.className;
    expect(cls).toContain('font-mono');
    expect(cls).toContain('uppercase');
    expect(cls).toContain('border-border-subtle');
    expect(cls).toContain('rounded-[var(--radius-field)]');
  });

  it('merges caller class string onto the defaults', () => {
    const { container } = render(Harness, {
      props: { extraClass: 'ml-auto custom-hook' },
    });
    const kbd = container.querySelector('kbd')!;
    expect(kbd.className).toContain('font-mono');
    expect(kbd.className).toContain('ml-auto');
    expect(kbd.className).toContain('custom-hook');
  });
});
