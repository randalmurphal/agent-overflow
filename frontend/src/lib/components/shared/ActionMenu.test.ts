// The shared action menu and the shown set it keeps per menu name
// (stores/menuShownActions.svelte.ts): the defaults at once and the rest
// behind Show more, a changed set saved on this device and heard from
// another window, an action run before the menu closes, and a page that
// opens on choosing its action, or at once when the menu opens at it.

import { fireEvent, render, waitFor, within } from '@testing-library/svelte';
import { createRawSnippet } from 'svelte';
import { describe, expect, it, vi } from 'vitest';
import ActionMenu, { type MenuGroup } from './ActionMenu.svelte';
import { shownActions } from '../../stores/menuShownActions.svelte';

const STORE_KEY = 'agent-overflow:frontend:menu-shown-actions';

const page = createRawSnippet(() => ({ render: () => '<p data-testid="the-page">Page</p>' }));

function groups(run: () => void): MenuGroup[] {
  return [
    { label: 'Open', actions: [{ id: 'open', label: 'Open', run }, { id: 'tracker', label: 'Open in the tracker', run }] },
    { label: 'Change', actions: [{ id: 'move', label: 'Move to…', page }, { id: 'type', label: 'Type…', run }] },
  ];
}

function labels(): string[] {
  return within(document.body).getAllByRole('menuitem').map((el) => el.textContent?.trim() ?? '');
}

function mount(props: { onDismiss?: () => void; run?: () => void; opening?: string } = {}) {
  return render(ActionMenu, {
    x: 10, y: 10, ariaLabel: 'Actions', name: 'test-menu', defaults: ['open', 'move'],
    groups: groups(props.run ?? (() => {})), opening: props.opening, onDismiss: props.onDismiss ?? (() => {}),
  });
}

describe('the action menu', () => {
  it('shows its defaults at once and the rest behind Show more, and saves a changed set', async () => {
    const view = mount();
    expect(labels()).toEqual(['Open', 'Move to…', 'Show more']);
    await fireEvent.click(view.getByTestId('action-menu-more'));
    expect(labels()).toEqual(['Open', 'Move to…', 'Open in the tracker', 'Type…', 'Choose what shows first…']);
    await fireEvent.click(view.getByTestId('action-menu-choose'));
    const items = () => view.getAllByTestId('action-menu-choose-item');
    expect(items().map((el) => el.getAttribute('aria-checked'))).toEqual(['true', 'false', 'true', 'false']);
    await fireEvent.click(items()[3]);
    await fireEvent.click(items()[0]);
    expect(JSON.parse(localStorage.getItem(STORE_KEY)!)).toEqual({ 'test-menu': ['move', 'type'] });
    await fireEvent.click(view.getByTestId('action-menu-choose-done'));
    expect(labels().slice(0, 2)).toEqual(['Move to…', 'Type…']);
    view.unmount();
    mount();
    expect(labels()).toEqual(['Move to…', 'Type…', 'Show more']);
  });

  it('takes a set another window saved', async () => {
    expect([...shownActions('test-menu', ['open'])]).toEqual(['open']);
    localStorage.setItem(STORE_KEY, JSON.stringify({ 'test-menu': ['type'] }));
    window.dispatchEvent(new StorageEvent('storage', { key: STORE_KEY, storageArea: localStorage }));
    expect([...shownActions('test-menu', ['open'])]).toEqual(['type']);
    localStorage.removeItem(STORE_KEY);
    window.dispatchEvent(new StorageEvent('storage', { key: STORE_KEY, storageArea: localStorage }));
    expect([...shownActions('test-menu', ['open'])]).toEqual(['open']);
  });

  it('runs an action before it closes, and opens a page in its place with Back', async () => {
    const order: string[] = [];
    const view = mount({ run: () => order.push('run'), onDismiss: () => order.push('dismiss') });
    await fireEvent.click(within(document.body).getByRole('menuitem', { name: 'Open' }));
    expect(order).toEqual(['run', 'dismiss']);
    order.length = 0;
    await fireEvent.click(within(document.body).getByRole('menuitem', { name: 'Move to…' }));
    expect(order).toEqual([]);
    expect(view.getByTestId('the-page')).toBeTruthy();
    expect(view.getByTestId('action-menu').dataset.mode).toBe('page');
    await fireEvent.click(view.getByTestId('action-menu-back'));
    await waitFor(() => expect(view.getByTestId('action-menu').dataset.mode).toBe('menu'));
  });

  it('opens at the page it is asked to, not at a hover of its row', async () => {
    const view = mount({ opening: 'move' });
    expect(view.getByTestId('the-page')).toBeTruthy();
    view.unmount();
    const run = vi.fn();
    const plain = mount({ run });
    await fireEvent.pointerEnter(within(document.body).getByRole('menuitem', { name: 'Move to…' }));
    await fireEvent.mouseEnter(within(document.body).getByRole('menuitem', { name: 'Move to…' }));
    expect(plain.queryByTestId('the-page')).toBeNull();
    expect(run).not.toHaveBeenCalled();
  });
});
