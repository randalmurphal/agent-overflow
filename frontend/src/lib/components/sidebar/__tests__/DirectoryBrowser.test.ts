import { beforeEach, describe, expect, it, vi } from 'vitest';
import { render, fireEvent } from '@testing-library/svelte';
import { tick } from 'svelte';
import DirectoryBrowser from '../DirectoryBrowser.svelte';
import { setBindingMock } from '../../../../test/mocks/bindings-app';
import type { DirectoryListing } from '../../../types/models';

function mkListing(overrides: Partial<DirectoryListing> = {}): DirectoryListing {
  return {
    path: '/Users/me',
    parent: '/Users',
    separator: '/',
    entries: [
      { name: 'code', isDir: true, hidden: false, isRepo: true },
      { name: 'docs', isDir: true, hidden: false, isRepo: false },
      { name: 'note.txt', isDir: false, hidden: false, isRepo: false },
    ],
    truncated: false,
    ...overrides,
  };
}

async function flushMount(): Promise<void> {
  // Effect fires -> RPC resolves -> state update -> paint.
  await Promise.resolve();
  for (let i = 0; i < 4; i += 1) await tick();
}

describe('<DirectoryBrowser>', () => {
  beforeEach(() => {
    setBindingMock('BrowseDirectory', async () => mkListing());
  });

  it('fetches the initial path on mount and renders entries', async () => {
    const browse = setBindingMock('BrowseDirectory', async () => mkListing());
    const { findAllByTestId } = render(DirectoryBrowser, {
      props: { initialPath: '~' },
    });
    await flushMount();
    expect(browse).toHaveBeenCalledWith('~');
    const entries = await findAllByTestId('directory-browser-entry');
    expect(entries).toHaveLength(3);
  });

  it('arrow keys move the selected highlight', async () => {
    const { findAllByTestId, getByTestId } = render(DirectoryBrowser, {
      props: { initialPath: '~' },
    });
    await flushMount();
    const list = getByTestId('directory-browser-list');
    const entriesBefore = await findAllByTestId('directory-browser-entry');
    expect(entriesBefore[0].getAttribute('aria-selected')).toBe('true');
    await fireEvent.keyDown(list, { key: 'ArrowDown' });
    await tick();
    const entriesAfter = await findAllByTestId('directory-browser-entry');
    expect(entriesAfter[0].getAttribute('aria-selected')).toBe('false');
    expect(entriesAfter[1].getAttribute('aria-selected')).toBe('true');
  });

  it('Enter on a directory drills in and calls onSelect with the new path', async () => {
    const onSelect = vi.fn();
    const browse = setBindingMock('BrowseDirectory', async (path: string) => {
      if (path === '~') return mkListing();
      return mkListing({
        path: '/Users/me/code',
        parent: '/Users/me',
        entries: [],
      });
    });
    const { getByTestId } = render(DirectoryBrowser, {
      props: { initialPath: '~', onSelect },
    });
    await flushMount();
    expect(onSelect).toHaveBeenLastCalledWith('/Users/me');
    const list = getByTestId('directory-browser-list');
    await fireEvent.keyDown(list, { key: 'Enter' });
    for (let i = 0; i < 4; i += 1) await tick();
    expect(browse).toHaveBeenLastCalledWith('/Users/me/code');
    expect(onSelect).toHaveBeenLastCalledWith('/Users/me/code');
  });

  it('Backspace goes back to the parent directory', async () => {
    const browse = setBindingMock('BrowseDirectory', async () => mkListing());
    const { getByTestId } = render(DirectoryBrowser, {
      props: { initialPath: '~' },
    });
    await flushMount();
    const list = getByTestId('directory-browser-list');
    await fireEvent.keyDown(list, { key: 'Backspace' });
    for (let i = 0; i < 4; i += 1) await tick();
    expect(browse).toHaveBeenLastCalledWith('/Users');
  });

  it('debounces path-input changes and calls BrowseDirectory once', async () => {
    vi.useFakeTimers();
    const browse = setBindingMock('BrowseDirectory', async () => mkListing());
    const { getByTestId } = render(DirectoryBrowser, {
      props: { initialPath: '~' },
    });
    // Flush initial fetch.
    await vi.advanceTimersByTimeAsync(0);
    for (let i = 0; i < 3; i += 1) await tick();
    const initialCalls = browse.mock.calls.length;
    const input = getByTestId('directory-browser-path') as HTMLInputElement;
    await fireEvent.input(input, { target: { value: '/t' } });
    await fireEvent.input(input, { target: { value: '/tm' } });
    await fireEvent.input(input, { target: { value: '/tmp' } });
    // No extra browse calls before the debounce elapses.
    expect(browse.mock.calls.length).toBe(initialCalls);
    await vi.advanceTimersByTimeAsync(120);
    await Promise.resolve();
    for (let i = 0; i < 3; i += 1) await tick();
    expect(browse.mock.calls.length).toBe(initialCalls + 1);
    expect(browse.mock.calls[initialCalls][0]).toBe('/tmp');
    vi.useRealTimers();
  });
});
