// Verifies the IconButton primitive's core contract:
//   - renders an icon-only <button> with aria-label + title.
//   - disabled state blocks clicks and sets aria-disabled on the native button.
//   - size / variant props drive the presentational classes we guarantee.
//
// The test uses a tiny .svelte harness because Snippet children can't be
// constructed from plain TS.

import { describe, expect, it, vi } from 'vitest';
import { render, fireEvent } from '@testing-library/svelte';
import Harness from './IconButtonHarness.svelte';

describe('<IconButton>', () => {
  it('renders a button with aria-label and tooltip derived from `label`', () => {
    const { getByRole } = render(Harness, { props: { label: 'Open settings' } });
    const button = getByRole('button');
    expect(button.getAttribute('aria-label')).toBe('Open settings');
    expect(button.getAttribute('title')).toBe('Open settings');
    expect(button.getAttribute('type')).toBe('button');
  });

  it('renders the icon child content', () => {
    const { getByTestId } = render(Harness, { props: { label: 'x' } });
    expect(getByTestId('icon')).toBeInTheDocument();
  });

  it('fires onClick when clicked', async () => {
    const onClick = vi.fn();
    const { getByRole } = render(Harness, { props: { label: 'x', onClick } });
    await fireEvent.click(getByRole('button'));
    expect(onClick).toHaveBeenCalledTimes(1);
  });

  it('does not fire onClick when disabled', async () => {
    const onClick = vi.fn();
    const { getByRole } = render(Harness, {
      props: { label: 'x', onClick, disabled: true },
    });
    const button = getByRole('button') as HTMLButtonElement;
    expect(button.disabled).toBe(true);
    await fireEvent.click(button);
    // The disabled attribute alone is enough to block the native click,
    // but we also guard inside the component — either way the spy
    // should not fire.
    expect(onClick).not.toHaveBeenCalled();
  });

  it('applies sm size class when size="sm"', () => {
    const { getByRole } = render(Harness, { props: { label: 'x', size: 'sm' } });
    const cls = getByRole('button').className;
    expect(cls).toContain('h-7');
    expect(cls).toContain('w-7');
  });

  it('applies md size class by default', () => {
    const { getByRole } = render(Harness, { props: { label: 'x' } });
    const cls = getByRole('button').className;
    expect(cls).toContain('h-8');
    expect(cls).toContain('w-8');
  });

  it('switches background class for variant="subtle"', () => {
    const { getByRole } = render(Harness, { props: { label: 'x', variant: 'subtle' } });
    const cls = getByRole('button').className;
    expect(cls).toContain('bg-surface-2/40');
  });
});
