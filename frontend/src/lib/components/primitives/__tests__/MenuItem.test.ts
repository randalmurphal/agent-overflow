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

  it.each([
    { name: 'click calls onSelect', event: 'click' as const, init: {} },
    { name: 'Enter key activates onSelect', event: 'keyDown' as const, init: { key: 'Enter' } },
    { name: 'Space key activates onSelect', event: 'keyDown' as const, init: { key: ' ' } },
  ])('$name', async ({ event, init }) => {
    const onSelect = vi.fn();
    const { getByRole } = render(Harness, { props: { label: 'Apple', onSelect } });
    await fireEvent[event](getByRole('menuitem'), init);
    expect(onSelect).toHaveBeenCalledTimes(1);
  });

  // Enter with Alt, Ctrl or Meta is an app chord (alt+shift+enter maximizes
  // the pane): the item neither acts nor takes the event from the dispatcher.
  it('leaves an Enter chord with Alt, Ctrl or Meta to the app', async () => {
    const onSelect = vi.fn();
    const { getByRole } = render(Harness, { props: { label: 'Close other tabs', onSelect } });
    for (const chord of [{ altKey: true, shiftKey: true }, { ctrlKey: true }, { metaKey: true }]) {
      const event = new KeyboardEvent('keydown', { key: 'Enter', bubbles: true, cancelable: true, ...chord });
      getByRole('menuitem').dispatchEvent(event);
      expect(event.defaultPrevented).toBe(false);
    }
    expect(onSelect).not.toHaveBeenCalled();
    await fireEvent.keyDown(getByRole('menuitem'), { key: 'Enter', shiftKey: true });
    expect(onSelect).toHaveBeenCalledTimes(1);
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

  it('row disabled leaves an enabled action clickable and undimmed', async () => {
    // The row select and the action are independent affordances: EnvPicker
    // blocks switching workspaces while a turn runs but still allows
    // removing idle worktrees via the row action.
    const onSelect = vi.fn();
    const onAction = vi.fn();
    const { getByRole, getByLabelText } = render(Harness, {
      props: { label: 'Apple', disabled: true, onSelect, onAction, showAction: true },
    });
    await fireEvent.click(getByRole('menuitem'));
    expect(onSelect).not.toHaveBeenCalled();

    const action = getByLabelText('Row action');
    expect(action).not.toBeDisabled();
    // Announced enabled despite the row's inherited aria-disabled="true".
    expect(action).toHaveAttribute('aria-disabled', 'false');
    // Dimming lives on the content wrapper, not the row container — an
    // ancestor's opacity would dim the enabled action with no way back.
    expect(getByRole('menuitem').className).not.toContain('opacity-50');
    expect(action.closest('.opacity-50')).toBeNull();
    await fireEvent.click(action);
    expect(onAction).toHaveBeenCalledTimes(1);
  });

  it.each([
    { name: 'actionDisabled blocks onAction independently of the row', props: { label: 'Apple', showAction: true } },
    { name: 'actionDisabled blocks a text action too', props: { label: 'atlassian', actionText: 'Sign in' } },
  ])('$name', async ({ props }) => {
    const onAction = vi.fn();
    const { getByLabelText } = render(Harness, {
      props: { ...props, onAction, actionDisabled: true },
    });
    const action = getByLabelText('Row action');
    expect(action).toBeDisabled();
    await fireEvent.click(action);
    expect(onAction).not.toHaveBeenCalled();
  });

  it('renders a labeled text action button and routes its click to onAction only', async () => {
    const onSelect = vi.fn();
    const onAction = vi.fn();
    const { getByLabelText, getByRole } = render(Harness, {
      props: { label: 'atlassian', actionText: 'Sign in again', onSelect, onAction },
    });
    const action = getByLabelText('Row action');
    expect(action.textContent).toBe('Sign in again');
    // The labeled variant sizes to its text instead of the icon square.
    expect(action.className).toContain('px-1.5');
    expect(action.className).not.toContain('w-5');
    await fireEvent.click(action);
    expect(onAction).toHaveBeenCalledTimes(1);
    // The action click must not bubble into the row select.
    expect(onSelect).not.toHaveBeenCalled();
    // A row hosting a nested action button cannot itself be a <button>.
    expect(getByRole('menuitem').tagName).toBe('DIV');
  });

  it('renders the text action at the start position when asked', () => {
    const { getByRole, getByLabelText } = render(Harness, {
      props: { label: 'atlassian', actionText: 'Sign in', actionPosition: 'start', onAction: vi.fn() },
    });
    const item = getByRole('menuitem');
    const action = getByLabelText('Row action');
    // Start-positioned action renders before the label content.
    expect(item.firstElementChild).toBe(action);
  });

  it('an actionText without onAction renders no button and keeps the row a <button>', () => {
    const { getByRole, queryByLabelText } = render(Harness, {
      props: { label: 'atlassian', actionText: 'Sign in' },
    });
    expect(queryByLabelText('Row action')).toBeNull();
    expect(getByRole('menuitem').tagName).toBe('BUTTON');
  });

  it.each([
    // U+2713 CHECK MARK.
    { name: 'renders the check glyph when checked=true', props: { checked: true }, text: '\u2713' },
    { name: 'renders the kbd hint when provided', props: { kbd: 'mod+K' }, text: 'mod+K' },
    { name: 'renders the suffix hint when provided', props: { suffix: 'remote' }, text: 'remote' },
  ])('$name', ({ props, text }) => {
    const { container } = render(Harness, { props: { label: 'Apple', ...props } });
    expect(container.textContent).toContain(text);
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
