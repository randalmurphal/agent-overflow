// MenuItem contract:
//   - role=menuitem + default tabindex=-1 (roving tabindex is Menu's job).
//   - click + Enter + Space all call onSelect.
//   - Enter dispatches a bubbling menuitem-select CustomEvent.
//   - disabled sets aria-disabled=true and blocks onSelect.
//   - checked renders the check glyph.
//   - danger variant applies the error text color class.

import { describe, expect, it, vi } from 'vitest';
import { render, fireEvent } from '@testing-library/svelte';
import Harness from './MenuItemHarness.svelte';

describe('<MenuItem>', () => {
  it('renders with role=menuitem and tabindex=-1', () => {
    const { getByRole } = render(Harness, { props: { label: 'Apple' } });
    const item = getByRole('menuitem');
    expect(item.tabIndex).toBe(-1);
    expect(item.textContent).toMatch(/Apple/);
  });

  it('click calls onSelect', async () => {
    const onSelect = vi.fn();
    const { getByRole } = render(Harness, { props: { label: 'Apple', onSelect } });
    await fireEvent.click(getByRole('menuitem'));
    expect(onSelect).toHaveBeenCalledTimes(1);
  });

  it('Enter key activates onSelect', async () => {
    const onSelect = vi.fn();
    const { getByRole } = render(Harness, { props: { label: 'Apple', onSelect } });
    await fireEvent.keyDown(getByRole('menuitem'), { key: 'Enter' });
    expect(onSelect).toHaveBeenCalled();
  });

  it('Space key activates onSelect', async () => {
    const onSelect = vi.fn();
    const { getByRole } = render(Harness, { props: { label: 'Apple', onSelect } });
    await fireEvent.keyDown(getByRole('menuitem'), { key: ' ' });
    expect(onSelect).toHaveBeenCalled();
  });

  it('dispatches a bubbling menuitem-select CustomEvent after onSelect', async () => {
    const { getByRole, container } = render(Harness, { props: { label: 'Apple' } });
    const spy = vi.fn();
    container.addEventListener('menuitem-select', spy);
    await fireEvent.click(getByRole('menuitem'));
    expect(spy).toHaveBeenCalled();
  });

  it('disabled sets aria-disabled and blocks onSelect', async () => {
    const onSelect = vi.fn();
    const { getByRole } = render(Harness, {
      props: { label: 'Apple', disabled: true, onSelect },
    });
    const item = getByRole('menuitem');
    expect(item.getAttribute('aria-disabled')).toBe('true');
    await fireEvent.click(item);
    expect(onSelect).not.toHaveBeenCalled();
  });

  it('renders the check glyph when checked=true', () => {
    const { container } = render(Harness, { props: { label: 'Apple', checked: true } });
    // 10003 is U+2713 CHECK MARK.
    expect(container.textContent).toContain('\u2713');
  });

  it('renders the kbd hint when provided', () => {
    const { container } = render(Harness, { props: { label: 'Apple', kbd: 'mod+K' } });
    expect(container.textContent).toContain('mod+K');
  });

  it('renders the suffix hint when provided', () => {
    const { container } = render(Harness, { props: { label: 'Apple', suffix: 'remote' } });
    expect(container.textContent).toContain('remote');
  });

  it('applies title when provided', () => {
    const { getByRole } = render(Harness, { props: { label: 'Apple', title: 'Unavailable' } });
    expect(getByRole('menuitem')).toHaveAttribute('title', 'Unavailable');
  });

  it('renders the icon slot when provided', () => {
    const { getByTestId } = render(Harness, { props: { label: 'Apple', showIcon: true } });
    expect(getByTestId('menuitem-icon')).toBeInTheDocument();
  });

  it('danger variant applies text-error class', () => {
    const { getByRole } = render(Harness, { props: { label: 'Delete', variant: 'danger' } });
    expect(getByRole('menuitem').className).toContain('text-error');
  });

  // Stage 1 redesign: tighter hover + accent token migration.
  it('default variant uses text-fg (not the legacy text-text-primary)', () => {
    const { getByRole } = render(Harness, { props: { label: 'Item' } });
    expect(getByRole('menuitem').className).toContain('text-fg');
    expect(getByRole('menuitem').className).not.toContain('text-text-primary');
  });

  it('uses the softer hover tint (bg-surface-2/40)', () => {
    const { getByRole } = render(Harness, { props: { label: 'Item' } });
    expect(getByRole('menuitem').className).toContain('hover:bg-surface-2/40');
  });

  it('renders indicator snippet instead of checkmark when indicator is provided', () => {
    const { container, getByTestId } = render(Harness, {
      props: { label: 'MCP', checked: true, showIndicator: true },
    });
    expect(getByTestId('menuitem-indicator')).toBeInTheDocument();
    expect(container.textContent).not.toContain('✓');
  });
});
