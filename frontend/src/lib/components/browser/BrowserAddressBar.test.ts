import { fireEvent, render } from '@testing-library/svelte';
import { tick } from 'svelte';
import { afterEach, describe, expect, it, vi } from 'vitest';
import BrowserAddressBar from './BrowserAddressBar.svelte';
import { getPromotedPaneId, resetPaneLayoutForTest, setPaneLayoutItemsForTest } from '../../stores/paneLayout.svelte';

const URL_A = 'https://example.test/a';

function page(extra: Record<string, unknown> = {}) {
  return { id: 'a', url: URL_A, title: 'Page a', canGoBack: false, canGoForward: false, ...extra };
}

function mount(activePage: ReturnType<typeof page> | null = page(), promotePaneId: string | null = null) {
  const onAct = vi.fn(async () => {});
  const onLeave = vi.fn();
  const onOpenFile = vi.fn();
  const view = render(BrowserAddressBar, {
    activePage: activePage as never,
    sizeLabel: '',
    promotePaneId,
    onAct,
    onRevealPageFile: async () => {},
    onOpenFile,
    onLeave,
  });
  const input = view.getByRole('textbox', { name: 'Address' }) as HTMLInputElement;
  return { view, input, onAct, onLeave, onOpenFile };
}

function selected(input: HTMLInputElement): [number | null, number | null] {
  return [input.selectionStart, input.selectionEnd];
}

describe('<BrowserAddressBar>', () => {
  afterEach(() => {
    resetPaneLayoutForTest();
  });

  it('selects the whole address when focused from the keyboard', async () => {
    const { input } = mount();
    input.focus();
    await tick();
    expect(document.activeElement).toBe(input);
    expect(selected(input)).toEqual([0, URL_A.length]);
  });

  it('selects the whole address on the click that focuses it, and a second click places the caret', async () => {
    const { input } = mount();
    // The first press focuses; the engine then places the caret.
    await fireEvent.pointerDown(input);
    input.focus();
    input.setSelectionRange(5, 5);
    await fireEvent.mouseUp(input);
    expect(selected(input)).toEqual([0, URL_A.length]);
    // A second press on the focused field is an ordinary caret placement.
    await fireEvent.pointerDown(input);
    input.setSelectionRange(8, 8);
    await fireEvent.mouseUp(input);
    expect(selected(input)).toEqual([8, 8]);
  });

  it('keeps a selection dragged out by the click that focused it', async () => {
    const { input } = mount();
    await fireEvent.pointerDown(input);
    input.focus();
    input.setSelectionRange(2, 6);
    await fireEvent.mouseUp(input);
    expect(selected(input)).toEqual([2, 6]);
  });

  it('Escape puts the page address back and leaves the field, taking the press', async () => {
    const { input, onLeave, onAct } = mount();
    input.focus();
    await fireEvent.input(input, { target: { value: 'half typed' } });
    const escape = new KeyboardEvent('keydown', { key: 'Escape', bubbles: true, cancelable: true });
    input.dispatchEvent(escape);
    await tick();
    expect(escape.defaultPrevented).toBe(true);
    expect(input.value).toBe(URL_A);
    expect(onLeave).toHaveBeenCalledTimes(1);
    expect(onAct).not.toHaveBeenCalled();
  });

  it('navigates on Enter and Shift+Enter, and leaves app chords and IME confirmation alone', async () => {
    const { input, onAct, onLeave } = mount();
    input.focus();
    await fireEvent.input(input, { target: { value: 'example.org' } });
    const press = (init: KeyboardEventInit) => {
      const event = new KeyboardEvent('keydown', { key: 'Enter', bubbles: true, cancelable: true, ...init });
      input.dispatchEvent(event);
      return event.defaultPrevented;
    };
    expect(press({ altKey: true, shiftKey: true })).toBe(false);
    expect(press({ ctrlKey: true })).toBe(false);
    expect(press({ metaKey: true })).toBe(false);
    expect(press({ isComposing: true })).toBe(false);
    expect(onAct).not.toHaveBeenCalled();
    expect(press({})).toBe(true);
    expect(onAct).toHaveBeenLastCalledWith({ kind: 'navigate', pageId: 'a', address: 'example.org' });
    expect(press({ shiftKey: true })).toBe(true);
    expect(onAct).toHaveBeenCalledTimes(2);
    expect(onLeave).toHaveBeenCalledTimes(2);
  });

  it("keeps an edit over its own page changing, and shows another tab's address once that tab is active", async () => {
    const { view, input } = mount();
    input.focus();
    await fireEvent.input(input, { target: { value: 'half typed' } });
    await view.rerender({ activePage: page({ title: 'Page a, loaded', url: 'https://example.test/a2' }) as never });
    expect(input.value).toBe('half typed');
    await view.rerender({ activePage: page({ id: 'b', url: 'https://example.test/b' }) as never });
    expect(input.value).toBe('https://example.test/b');
    expect(document.activeElement).toBe(input);
  });

  // A draft belongs to the tab it was typed on: after a switch, Enter
  // navigates the tab now shown to its own address, never to the draft.
  it("does not submit a tab's draft to the tab switched to", async () => {
    const { view, input, onAct } = mount(page({ id: 'b', url: 'https://example.test/b' }));
    input.focus();
    await fireEvent.input(input, { target: { value: 'https://unsubmitted-for-b.test/' } });
    await view.rerender({ activePage: page() as never });
    await tick();
    await fireEvent.keyDown(input, { key: 'Enter' });
    expect(onAct).toHaveBeenLastCalledWith({ kind: 'navigate', pageId: 'a', address: URL_A });
  });

  it('puts the page address back when the field loses focus with an edit, on a settled tab and after a switch', async () => {
    const { view, input } = mount();
    input.focus();
    await fireEvent.input(input, { target: { value: 'half typed' } });
    input.blur();
    await tick();
    expect(input.value).toBe(URL_A);

    await view.rerender({ activePage: page({ id: 'b', url: 'https://example.test/b' }) as never });
    input.focus();
    await fireEvent.input(input, { target: { value: 'typed for b' } });
    input.blur();
    await tick();
    expect(input.value).toBe('https://example.test/b');
  });

  it('turns reload into stop while the page loads, and back', async () => {
    const { view, onAct } = mount(page({ loading: true }));
    expect(view.queryByRole('button', { name: 'Reload' })).toBeNull();
    view.getByRole('button', { name: 'Stop' }).click();
    expect(onAct).toHaveBeenLastCalledWith({ kind: 'stop', pageId: 'a' });
    await view.rerender({ activePage: page() as never });
    expect(view.queryByRole('button', { name: 'Stop' })).toBeNull();
    view.getByRole('button', { name: 'Reload' }).click();
    expect(onAct).toHaveBeenLastCalledWith({ kind: 'reload', pageId: 'a' });
  });

  it('takes no reload for a tab with no page yet', () => {
    const { view } = mount(page({ creating: true }));
    expect((view.getByRole('button', { name: 'Reload' }) as HTMLButtonElement).disabled).toBe(true);
  });

  it('opens the file picker', () => {
    const { view, onOpenFile } = mount();
    view.getByRole('button', { name: 'Open file' }).click();
    expect(onOpenFile).toHaveBeenCalledTimes(1);
  });

  it('maximizes and restores the browser pane next to its close button', async () => {
    setPaneLayoutItemsForTest([
      { id: 'main', paneId: 'main', kind: 'thread', widthPx: 400 },
      { id: 'browser-main', paneId: 'browser-main', kind: 'browser', widthPx: 400 },
    ]);
    const { view } = mount(page(), 'browser-main');
    const promote = view.getByTestId('browser-promote');
    expect(promote.nextElementSibling?.getAttribute('data-testid')).toBe('browser-close');
    expect(promote.getAttribute('aria-label')).toBe('Maximize Pane');
    promote.click();
    await tick();
    expect(getPromotedPaneId()).toBe('browser-main');
    expect(promote.getAttribute('aria-label')).toBe('Restore Pane');
    promote.click();
    await tick();
    expect(getPromotedPaneId()).toBeNull();
  });

  it('offers no maximize without a pane to promote', () => {
    const { view } = mount(page(), null);
    expect(view.queryByTestId('browser-promote')).toBeNull();
  });
});
