import { fireEvent, render, waitFor } from '@testing-library/svelte';
import { tick } from 'svelte';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import BrowserAddressBarPaneHarness from './BrowserAddressBarPaneHarness.svelte';
import { resetBindingMocks, setBindingMock } from '../../../test/mocks/bindings-app';
import { applyBrowserCompanionState, resetBrowserCompanionForTest } from '../../stores/browserCompanion.svelte';
import { resetDevServersForTest } from '../../stores/devServers.svelte';
import { airspaceSurfaces, resetAirspaceForTest } from '../../utils/paneAirspace.svelte';

const THREAD = 'thread-1';

function page(id: string, url: string, title = id) {
  return { id, url, title, canGoBack: false, canGoForward: false };
}

const PAGES = [page('a', 'https://example.test/a', 'Page a'), page('b', 'http://localhost:5173/settings', 'Settings')];

function seed(activePageId = 'a') {
  applyBrowserCompanionState({ kind: 'state', threadId: THREAD, activePageId, visible: true, jar: 'worktree', pages: PAGES } as never);
}

function mount(activePage = PAGES[0]) {
  const onAct = vi.fn(async (..._args: unknown[]) => {});
  const onLeave = vi.fn();
  const view = render(BrowserAddressBarPaneHarness, {
    threadId: THREAD,
    activePage: activePage as never,
    sizeLabel: '',
    promotePaneId: null,
    onAct,
    onRevealPageFile: async () => {},
    onOpenFile: () => {},
    onLeave,
  });
  const input = view.getByRole('textbox', { name: 'Address' }) as HTMLInputElement;
  return { view, input, onAct, onLeave };
}

function key(input: HTMLInputElement, init: KeyboardEventInit): KeyboardEvent {
  const event = new KeyboardEvent('keydown', { bubbles: true, cancelable: true, ...init });
  input.dispatchEvent(event);
  return event;
}

async function type(input: HTMLInputElement, text: string): Promise<void> {
  await fireEvent.input(input, { target: { value: text } });
  await tick();
}

function rows(): HTMLElement[] {
  return Array.from(document.querySelectorAll<HTMLElement>('[data-testid="browser-address-suggestion"]'));
}

function rowText(): string[] {
  return rows().map((row) => `${row.dataset.kind} ${row.title}`);
}

describe('<BrowserAddressSuggestions>', () => {
  let recent: ReturnType<typeof vi.fn>;
  beforeEach(() => {
    resetBrowserCompanionForTest();
    resetDevServersForTest();
    resetAirspaceForTest();
    recent = setBindingMock('BrowserCompanionRecentAddresses', vi.fn(async () => [
      { url: 'https://example.test/a', title: 'Page a' },
      { url: 'https://github.test/org/repo', title: 'Repo' },
    ]));
    setBindingMock('GetDevServers', async () => ({
      previewHost: '',
      servers: [{ port: 8080, threadId: THREAD, process: 'node', listening: true, allowed: false, source: 'seen', scheme: 'http' }],
    }));
    seed();
  });
  afterEach(() => {
    resetBrowserCompanionForTest();
    resetDevServersForTest();
    resetBindingMocks();
    resetAirspaceForTest();
  });

  it('reads the recent addresses and dev servers on focus and offers them for typed text', async () => {
    const { input } = mount();
    input.focus();
    await waitFor(() => expect(recent).toHaveBeenCalledWith(THREAD));
    expect(rows()).toEqual([]);
    await type(input, 'local');
    await waitFor(() => expect(rowText()).toEqual(['tab http://localhost:5173/settings', 'dev-server http://localhost:8080/']));
    await type(input, 'test');
    // The field's own tab, and its address among the recent ones, are not offered.
    expect(rowText()).toEqual(['recent https://github.test/org/repo']);
    expect(input.value).toBe('test');
    await type(input, '');
    expect(rows()).toEqual([]);
  });

  it('moves with the arrows without touching the typed text, and Enter takes the highlighted row', async () => {
    const { input, onAct, onLeave } = mount();
    input.focus();
    await type(input, 'o');
    await waitFor(() => expect(rows().length).toBe(3));
    expect(key(input, { key: 'ArrowDown' }).defaultPrevented).toBe(true);
    await tick();
    expect(rows().map((row) => row.getAttribute('aria-selected'))).toEqual(['true', 'false', 'false']);
    expect(input.getAttribute('aria-activedescendant')).toBe(rows()[0].id);
    key(input, { key: 'ArrowUp' });
    await tick();
    // Up from the first row returns to the typed text.
    expect(rows().map((row) => row.getAttribute('aria-selected'))).toEqual(['false', 'false', 'false']);
    expect(input.hasAttribute('aria-activedescendant')).toBe(false);
    key(input, { key: 'ArrowUp' });
    await tick();
    expect(rows()[2].getAttribute('aria-selected')).toBe('true');
    expect(input.value).toBe('o');
    expect(key(input, { key: 'Enter' }).defaultPrevented).toBe(true);
    await tick();
    expect(onAct).toHaveBeenCalledWith('navigate', 'a', 'https://github.test/org/repo');
    expect(onLeave).toHaveBeenCalledTimes(1);
    expect(rows()).toEqual([]);
  });

  it('switches to a tab, then leaves the field for it', async () => {
    let finish: () => void = () => {};
    const { input, onAct, onLeave } = mount();
    onAct.mockImplementation(() => new Promise<void>((resolve) => (finish = resolve)));
    input.focus();
    await type(input, 'settings');
    await waitFor(() => expect(rows().length).toBe(1));
    key(input, { key: 'ArrowDown' });
    await tick();
    key(input, { key: 'Enter' });
    expect(onAct).toHaveBeenCalledWith('activate', 'b');
    expect(onLeave).not.toHaveBeenCalled();
    finish();
    await waitFor(() => expect(onLeave).toHaveBeenCalledTimes(1));
  });

  it('does not take focus back after a tab switch if the user moved on', async () => {
    let finish: () => void = () => {};
    const { input, onAct, onLeave } = mount();
    onAct.mockImplementation(() => new Promise<void>((resolve) => (finish = resolve)));
    input.focus();
    await type(input, 'settings');
    await waitFor(() => expect(rows().length).toBe(1));
    key(input, { key: 'ArrowDown' });
    key(input, { key: 'Enter' });
    input.blur();
    finish();
    await Promise.resolve();
    await tick();
    expect(onLeave).not.toHaveBeenCalled();
  });

  it('Enter with no row highlighted navigates the typed text', async () => {
    const { input, onAct } = mount();
    input.focus();
    await type(input, 'local');
    await waitFor(() => expect(rows().length).toBe(2));
    key(input, { key: 'Enter' });
    expect(onAct).toHaveBeenCalledWith('navigate', 'a', 'local');
  });

  it('Escape closes the list first and reverts on the next press', async () => {
    const { input, onLeave } = mount();
    input.focus();
    await type(input, 'local');
    await waitFor(() => expect(rows().length).toBe(2));
    const first = key(input, { key: 'Escape' });
    await tick();
    expect(first.defaultPrevented).toBe(true);
    expect(rows()).toEqual([]);
    expect(input.value).toBe('local');
    expect(onLeave).not.toHaveBeenCalled();
    const second = key(input, { key: 'Escape' });
    await tick();
    expect(second.defaultPrevented).toBe(true);
    expect(input.value).toBe('https://example.test/a');
    expect(onLeave).toHaveBeenCalledTimes(1);
  });

  it('takes a clicked row without moving focus off the field first', async () => {
    const { input, onAct } = mount();
    input.focus();
    await type(input, 'github');
    await waitFor(() => expect(rows().length).toBe(1));
    const press = new MouseEvent('mousedown', { bubbles: true, cancelable: true });
    rows()[0].dispatchEvent(press);
    expect(press.defaultPrevented).toBe(true);
    await fireEvent.mouseMove(rows()[0]);
    expect(rows()[0].getAttribute('aria-selected')).toBe('true');
    await fireEvent.click(rows()[0]);
    expect(onAct).toHaveBeenCalledWith('navigate', 'a', 'https://github.test/org/repo');
  });

  it('paints as an overlay the native view yields to, only while open', async () => {
    const { input } = mount();
    input.focus();
    expect(airspaceSurfaces().length).toBe(0);
    await type(input, 'local');
    await waitFor(() => expect(rows().length).toBe(2));
    expect(airspaceSurfaces().length).toBe(1);
    expect(airspaceSurfaces()[0].contains(rows()[0])).toBe(true);
    input.blur();
    await tick();
    expect(rows()).toEqual([]);
    expect(airspaceSurfaces().length).toBe(0);
  });

  it('closes when another tab becomes active', async () => {
    const { view, input } = mount();
    input.focus();
    await type(input, 'local');
    await waitFor(() => expect(rows().length).toBe(2));
    seed('b');
    await view.rerender({ activePage: PAGES[1] as never });
    await tick();
    expect(rows()).toEqual([]);
  });

  it('says when the recent addresses could not be read', async () => {
    setBindingMock('BrowserCompanionRecentAddresses', async () => {
      throw new Error('browser manager unavailable');
    });
    const { view, input } = mount();
    input.focus();
    await tick();
    await type(input, 'local');
    expect((await view.findByTestId('browser-address-suggestions-error')).textContent).toContain('browser manager unavailable');
  });

  it('keeps the newest focus\'s read when an older one answers late', async () => {
    const answers: ((list: { url: string; title: string }[]) => void)[] = [];
    setBindingMock('BrowserCompanionRecentAddresses', () => new Promise((resolve) => answers.push(resolve)));
    const { input } = mount();
    input.focus();
    input.blur();
    input.focus();
    await waitFor(() => expect(answers.length).toBe(2));
    answers[1]([{ url: 'https://new.test/', title: '' }]);
    await tick();
    answers[0]([{ url: 'https://old.test/', title: '' }]);
    await tick();
    await type(input, '.test');
    await waitFor(() => expect(rowText()).toContain('recent https://new.test/'));
    expect(rowText()).not.toContain('recent https://old.test/');
  });
});
