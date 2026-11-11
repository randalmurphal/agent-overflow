import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { fireEvent, render, waitFor } from '@testing-library/svelte';
import { tick } from 'svelte';
import PaneFindBar from './PaneFindBar.svelte';
import {
  closePaneFind,
  getPaneFind,
  openPaneFind,
  registerPaneFindProvider,
  resetPaneFindForTest,
} from '../../stores/paneFind.svelte';
import { resetPaneLayoutForTest, setPaneLayoutItemsForTest } from '../../stores/paneLayout.svelte';
import { fakeFindProvider, type FakeFindProvider } from '../../../test/helpers/paneFind.svelte';

let provider: FakeFindProvider;
let fallback: HTMLButtonElement;

beforeEach(() => {
  resetPaneFindForTest();
  setPaneLayoutItemsForTest([{ id: 'a', paneId: 'a', kind: 'thread', widthPx: 600 }]);
  provider = fakeFindProvider();
  registerPaneFindProvider('a', provider);
  fallback = document.createElement('button');
  document.body.appendChild(fallback);
});

afterEach(() => {
  resetPaneFindForTest();
  resetPaneLayoutForTest();
  document.body.replaceChildren();
});

function renderBar() {
  return render(PaneFindBar, { paneId: 'a', title: 'My thread', fallbackFocus: () => fallback });
}

async function openBar(returnFocus: HTMLElement | null = null) {
  openPaneFind('a', { returnFocus });
  await tick();
}

describe('<PaneFindBar>', () => {
  it('renders nothing until its pane opens find, then a labelled search landmark with a focused input', async () => {
    const { queryByTestId, getByRole, getByTestId } = renderBar();
    expect(queryByTestId('pane-find-bar')).toBeNull();
    await openBar();
    expect(getByRole('search', { name: 'Find in My thread' })).toBe(getByTestId('pane-find-bar'));
    const input = getByTestId('pane-find-input') as HTMLInputElement;
    expect(input.placeholder).toBe('Find in thread');
    await waitFor(() => expect(document.activeElement).toBe(input));
  });

  it('shows nothing for an empty query, a spinner while pending, the position, then No results', async () => {
    const { getByTestId } = renderBar();
    await openBar();
    const count = getByTestId('pane-find-count');
    expect(count.getAttribute('aria-live')).toBe('polite');
    expect(count.textContent?.trim()).toBe('');

    await fireEvent.input(getByTestId('pane-find-input'), { target: { value: 'needle' } });
    expect(provider.search).toHaveBeenLastCalledWith('needle');
    provider.set({ pending: true });
    await tick();
    expect(count.querySelector('[data-testid="pane-find-spinner"]')).not.toBeNull();

    provider.set({ pending: false, count: 41, current: 2 });
    await tick();
    expect(count.textContent?.trim()).toBe('3/41');

    provider.set({ count: 1000, current: 0, capped: true });
    await tick();
    expect(count.textContent?.trim()).toBe('1/1000+');

    provider.set({ count: 0, current: -1, capped: false });
    await tick();
    expect(count.textContent?.trim()).toBe('No results');
  });

  it('disables stepping at zero results and steps with the buttons', async () => {
    const { getByTestId } = renderBar();
    await openBar();
    const prev = getByTestId('pane-find-previous') as HTMLButtonElement;
    const next = getByTestId('pane-find-next') as HTMLButtonElement;
    expect(prev.disabled).toBe(true);
    expect(next.disabled).toBe(true);
    provider.set({ count: 2, current: 0 });
    await tick();
    expect(prev.disabled).toBe(false);
    await fireEvent.click(next);
    await fireEvent.click(prev);
    expect(provider.next).toHaveBeenCalledTimes(1);
    expect(provider.previous).toHaveBeenCalledTimes(1);
  });

  it('Enter steps forward, Shift+Enter back, and IME composition is left alone', async () => {
    const { getByTestId } = renderBar();
    await openBar();
    provider.set({ count: 3, current: 0 });
    await tick();
    const input = getByTestId('pane-find-input');
    await fireEvent.keyDown(input, { key: 'Enter' });
    await fireEvent.keyDown(input, { key: 'Enter', shiftKey: true });
    await fireEvent.keyDown(input, { key: 'Enter', isComposing: true });
    expect(provider.next).toHaveBeenCalledTimes(1);
    expect(provider.previous).toHaveBeenCalledTimes(1);
  });

  it('a close clears and returns focus to where it was', async () => {
    const before = document.createElement('textarea');
    document.body.appendChild(before);
    before.focus();
    const { getByTestId, queryByTestId } = renderBar();
    await openBar(before);
    const input = getByTestId('pane-find-input');
    await waitFor(() => expect(document.activeElement).toBe(input));
    closePaneFind('a');
    await tick();
    expect(queryByTestId('pane-find-bar')).toBeNull();
    expect(provider.clear).toHaveBeenCalled();
    await waitFor(() => expect(document.activeElement).toBe(before));
  });

  // `find.close` decides which find Escape dismisses (builtinCommands tests);
  // the bar keeps only an IME composition's Escape from reaching it.
  it('leaves Escape to the find.close command, except one that ends an IME composition', async () => {
    const { getByTestId } = renderBar();
    await openBar();
    const input = getByTestId('pane-find-input');
    const seen: KeyboardEvent[] = [];
    const onKey = (event: KeyboardEvent) => seen.push(event);
    window.addEventListener('keydown', onKey);
    try {
      const plain = new KeyboardEvent('keydown', { key: 'Escape', bubbles: true, cancelable: true });
      input.dispatchEvent(plain);
      expect(seen).toEqual([plain]);
      expect(plain.defaultPrevented).toBe(false);
      expect(getPaneFind('a')?.open).toBe(true);
      input.dispatchEvent(new KeyboardEvent('keydown', { key: 'Escape', isComposing: true, bubbles: true, cancelable: true }));
      expect(seen).toEqual([plain]);
    } finally {
      window.removeEventListener('keydown', onKey);
    }
  });

  it('returns focus to the pane when the element it came from is gone', async () => {
    const before = document.createElement('textarea');
    document.body.appendChild(before);
    const { getByTestId } = renderBar();
    await openBar(before);
    before.remove();
    await fireEvent.click(getByTestId('pane-find-close'));
    await tick();
    await waitFor(() => expect(document.activeElement).toBe(fallback));
  });

  it('a reopen selects the last query', async () => {
    const { getByTestId } = renderBar();
    await openBar();
    await fireEvent.input(getByTestId('pane-find-input'), { target: { value: 'kept' } });
    closePaneFind('a');
    await tick();
    await openBar();
    const input = getByTestId('pane-find-input') as HTMLInputElement;
    expect(input.value).toBe('kept');
    await waitFor(() => expect(document.activeElement).toBe(input));
    expect([input.selectionStart, input.selectionEnd]).toEqual([0, 4]);
  });

  it('Ctrl+F while open refocuses and selects the input', async () => {
    const { getByTestId } = renderBar();
    await openBar();
    const input = getByTestId('pane-find-input') as HTMLInputElement;
    await fireEvent.input(input, { target: { value: 'again' } });
    fallback.focus();
    openPaneFind('a');
    await tick();
    await waitFor(() => expect(document.activeElement).toBe(input));
    expect([input.selectionStart, input.selectionEnd]).toEqual([0, 5]);
  });
});
