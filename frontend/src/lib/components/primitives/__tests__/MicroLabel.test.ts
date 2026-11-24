// Verifies the MicroLabel primitive's contract:
//   - renders as <span> by default, or the requested tag via `as`
//   - applies the uppercase/tracking-[0.18em]/text-fg-subtle treatment
//   - caller class merges onto the defaults

import { describe, expect, it } from 'vitest';
import { render } from '@testing-library/svelte';
import Harness from './MicroLabelHarness.svelte';

describe('<MicroLabel>', () => {
  it.each([
    { name: 'defaults to rendering as a <span>', props: { label: 'Projects' }, tag: 'SPAN' },
    { name: 'renders as the tag passed via `as`', props: { as: 'h2' as const, label: 'Section' }, tag: 'H2' },
  ])('$name', ({ props, tag }) => {
    const { container } = render(Harness, { props });
    const el = container.firstElementChild!;
    expect(el.tagName).toBe(tag);
    expect(el.textContent).toContain(props.label);
  });

  it('applies the uppercase + tracked + muted-text treatment', () => {
    const { container } = render(Harness);
    const el = container.firstElementChild!;
    const cls = el.className;
    expect(cls).toContain('uppercase');
    expect(cls).toContain('tracking-[0.18em]');
    expect(cls).toContain('text-fg-subtle');
  });

  it('merges caller class onto the defaults', () => {
    const { container } = render(Harness, {
      props: { extraClass: 'flex-1 custom-hook' },
    });
    const el = container.firstElementChild!;
    expect(el.className).toContain('uppercase');
    expect(el.className).toContain('flex-1');
    expect(el.className).toContain('custom-hook');
  });
});
