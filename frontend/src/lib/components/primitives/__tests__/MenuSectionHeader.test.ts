// MenuSectionHeader is presentational; we check the label renders and
// AT consumers are told to skip it (role=presentation).

import { describe, expect, it } from 'vitest';
import { render } from '@testing-library/svelte';
import MenuSectionHeader from '../MenuSectionHeader.svelte';

describe('<MenuSectionHeader>', () => {
  it('renders the provided label', () => {
    const { getByText } = render(MenuSectionHeader, { props: { label: 'Effort' } });
    expect(getByText('Effort')).toBeInTheDocument();
  });

  it('marks itself as presentation for screen readers', () => {
    const { container } = render(MenuSectionHeader, { props: { label: 'Effort' } });
    const el = container.querySelector('[data-menu-section-header]');
    expect(el?.getAttribute('role')).toBe('presentation');
  });
});
