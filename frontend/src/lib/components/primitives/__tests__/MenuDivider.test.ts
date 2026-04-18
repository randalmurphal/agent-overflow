// MenuDivider is pure visual, but its role=separator is important for
// screen readers so we keep a guard test.

import { describe, expect, it } from 'vitest';
import { render } from '@testing-library/svelte';
import MenuDivider from '../MenuDivider.svelte';

describe('<MenuDivider>', () => {
  it('renders with role=separator and horizontal orientation', () => {
    const { getByRole } = render(MenuDivider);
    const sep = getByRole('separator');
    expect(sep.getAttribute('aria-orientation')).toBe('horizontal');
  });

  it('uses the border-color theme token', () => {
    const { container } = render(MenuDivider);
    const sep = container.querySelector('[data-menu-divider]');
    expect(sep).not.toBeNull();
    expect(sep!.className).toContain('border-border');
  });
});
