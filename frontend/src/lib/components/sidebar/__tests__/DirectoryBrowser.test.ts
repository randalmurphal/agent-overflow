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
    exists: true,
    ...overrides,
  };
}

// "Nothing to list" shape — missing path or path-is-a-file. No error
// thrown; server returns exists=false so logs stay quiet on every
// in-progress keystroke.
function mkMissing(path: string): DirectoryListing {
  return {
    path,
    parent: '',
    separator: '/',
    entries: [],
    truncated: false,
    exists: false,
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

  it('selects file rows by click or Enter when a file callback is provided', async () => {
    const onSelectFile = vi.fn();
    const { getByTestId, findAllByTestId } = render(DirectoryBrowser, {
      props: { initialPath: '~', onSelectFile },
    });
    await flushMount();
    const entries = await findAllByTestId('directory-browser-entry');
    await fireEvent.click(entries[2]);
    expect(onSelectFile).toHaveBeenLastCalledWith('/Users/me/note.txt');

    onSelectFile.mockClear();
    const list = getByTestId('directory-browser-list');
    await fireEvent.keyDown(list, { key: 'Enter' });
    expect(onSelectFile).toHaveBeenLastCalledWith('/Users/me/note.txt');
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

  it('preserves user input when typing — does not overwrite with server-normalized path', async () => {
    vi.useFakeTimers();
    // Server strips the trailing slash from the typed path. The client
    // must NOT push that canonical form back into the input while the
    // user is still typing — their cursor would jump back.
    setBindingMock('BrowseDirectory', async (path: string) => {
      const clean = path.replace(/\/+$/, '') || '/';
      return mkListing({ path: clean, parent: '/Users' });
    });
    const { getByTestId } = render(DirectoryBrowser, {
      props: { initialPath: '~' },
    });
    await vi.advanceTimersByTimeAsync(0);
    for (let i = 0; i < 3; i += 1) await tick();

    const input = getByTestId('directory-browser-path') as HTMLInputElement;
    await fireEvent.input(input, { target: { value: '/Users/me/' } });
    await vi.advanceTimersByTimeAsync(120);
    await Promise.resolve();
    for (let i = 0; i < 3; i += 1) await tick();
    // User typed '/Users/me/' — server cleaned to '/Users/me' — the input
    // must still show the user's trailing slash so they can keep typing.
    expect(input.value).toBe('/Users/me/');
    vi.useRealTimers();
  });

  it('wipes the listing and shows a muted "No matches" hint when neither the typed path nor its parent resolve', async () => {
    vi.useFakeTimers();
    setBindingMock('BrowseDirectory', async (path: string) => {
      if (path === '~') return mkListing();
      // Server returns exists=false (not a thrown error) for both the
      // typed path and its parent — both are missing.
      return mkMissing(path);
    });
    const { getByTestId, queryByTestId } = render(DirectoryBrowser, {
      props: { initialPath: '~' },
    });
    await vi.advanceTimersByTimeAsync(0);
    for (let i = 0; i < 3; i += 1) await tick();

    const input = getByTestId('directory-browser-path') as HTMLInputElement;
    // Both "/nope/still-nope" AND its parent "/nope" fail — full wipe.
    await fireEvent.input(input, { target: { value: '/nope/still-nope' } });
    await vi.advanceTimersByTimeAsync(120);
    await Promise.resolve();
    for (let i = 0; i < 4; i += 1) await tick();

    expect(queryByTestId('directory-browser-no-matches')).not.toBeNull();
    expect(queryByTestId('directory-browser-error')).toBeNull();
    expect(queryByTestId('directory-browser-entry')).toBeNull();
    vi.useRealTimers();
  });

  it('falls back to prefix-filtering the parent directory when the typed path is incomplete', async () => {
    vi.useFakeTimers();
    // Direct browse of "/Users/randy/rep" returns exists=false, but
    // "/Users/randy" is a real directory containing "repos", "rover",
    // and "docs".
    setBindingMock('BrowseDirectory', async (path: string) => {
      if (path === '~') return mkListing();
      if (path === '/Users/randy') {
        return mkListing({
          path: '/Users/randy',
          parent: '/Users',
          entries: [
            { name: 'docs', isDir: true, hidden: false, isRepo: false },
            { name: 'repos', isDir: true, hidden: false, isRepo: false },
            { name: 'rover', isDir: true, hidden: false, isRepo: false },
          ],
        });
      }
      return mkMissing(path);
    });
    const { getByTestId, findAllByTestId, queryByTestId } = render(
      DirectoryBrowser,
      { props: { initialPath: '~' } },
    );
    await vi.advanceTimersByTimeAsync(0);
    for (let i = 0; i < 3; i += 1) await tick();

    const input = getByTestId('directory-browser-path') as HTMLInputElement;
    await fireEvent.input(input, { target: { value: '/Users/randy/rep' } });
    await vi.advanceTimersByTimeAsync(120);
    await Promise.resolve();
    for (let i = 0; i < 4; i += 1) await tick();

    // Filtered to entries whose name starts with "rep" — only "repos".
    const entries = await findAllByTestId('directory-browser-entry');
    expect(entries.map((e) => e.textContent?.trim())).toEqual(
      expect.arrayContaining([expect.stringContaining('repos')]),
    );
    expect(entries).toHaveLength(1);
    // No "no matches" hint — we found a match via prefix filter.
    expect(queryByTestId('directory-browser-no-matches')).toBeNull();
    expect(queryByTestId('directory-browser-error')).toBeNull();
    vi.useRealTimers();
  });

  it('reports onSelect("") when prefix-filtering so the parent modal can disable the Add button', async () => {
    vi.useFakeTimers();
    const onSelect = vi.fn();
    setBindingMock('BrowseDirectory', async (path: string) => {
      if (path === '~') return mkListing();
      if (path === '/Users/me') {
        return mkListing({
          path: '/Users/me',
          parent: '/Users',
          entries: [{ name: 'repos', isDir: true, hidden: false, isRepo: false }],
        });
      }
      return mkMissing(path);
    });
    const { getByTestId } = render(DirectoryBrowser, {
      props: { initialPath: '~', onSelect },
    });
    await vi.advanceTimersByTimeAsync(0);
    for (let i = 0; i < 3; i += 1) await tick();
    onSelect.mockClear();

    const input = getByTestId('directory-browser-path') as HTMLInputElement;
    await fireEvent.input(input, { target: { value: '/Users/me/rep' } });
    await vi.advanceTimersByTimeAsync(120);
    await Promise.resolve();
    for (let i = 0; i < 4; i += 1) await tick();

    expect(onSelect).toHaveBeenCalledWith('');
    vi.useRealTimers();
  });

  it('drilling into a prefix-filtered entry commits to the real path and updates onSelect', async () => {
    vi.useFakeTimers();
    const onSelect = vi.fn();
    setBindingMock('BrowseDirectory', async (path: string) => {
      if (path === '~') return mkListing();
      if (path === '/Users/me') {
        return mkListing({
          path: '/Users/me',
          parent: '/Users',
          entries: [{ name: 'repos', isDir: true, hidden: false, isRepo: false }],
        });
      }
      if (path === '/Users/me/repos') {
        return mkListing({
          path: '/Users/me/repos',
          parent: '/Users/me',
          entries: [],
        });
      }
      return mkMissing(path);
    });
    const { getByTestId, findAllByTestId } = render(DirectoryBrowser, {
      props: { initialPath: '~', onSelect },
    });
    await vi.advanceTimersByTimeAsync(0);
    for (let i = 0; i < 3; i += 1) await tick();

    const input = getByTestId('directory-browser-path') as HTMLInputElement;
    await fireEvent.input(input, { target: { value: '/Users/me/rep' } });
    await vi.advanceTimersByTimeAsync(120);
    await Promise.resolve();
    for (let i = 0; i < 4; i += 1) await tick();
    onSelect.mockClear();

    const entries = await findAllByTestId('directory-browser-entry');
    await fireEvent.click(entries[0]);
    for (let i = 0; i < 4; i += 1) await tick();

    expect(onSelect).toHaveBeenLastCalledWith('/Users/me/repos');
    expect(input.value).toBe('/Users/me/repos');
    vi.useRealTimers();
  });

  it('surfaces a clean one-line error on explicit commit (Enter) of an invalid path — not the raw JSON', async () => {
    vi.useFakeTimers();
    setBindingMock('BrowseDirectory', async (path: string) => {
      if (path === '~') return mkListing();
      return mkMissing(path);
    });
    const { getByTestId, queryByTestId } = render(DirectoryBrowser, {
      props: { initialPath: '~' },
    });
    await vi.advanceTimersByTimeAsync(0);
    for (let i = 0; i < 3; i += 1) await tick();

    const input = getByTestId('directory-browser-path') as HTMLInputElement;
    await fireEvent.input(input, { target: { value: '/Users/does-not-exist' } });
    // Commit without waiting for the debounce — Enter bypasses it.
    await fireEvent.keyDown(input, { key: 'Enter' });
    for (let i = 0; i < 4; i += 1) await tick();

    const errorEl = queryByTestId('directory-browser-error');
    expect(errorEl).not.toBeNull();
    const text = errorEl!.textContent ?? '';
    // Clean message, no JSON, no "RuntimeError" wrapping.
    expect(text).toContain('No such directory');
    expect(text).not.toContain('{');
    expect(text).not.toContain('RuntimeError');
    vi.useRealTimers();
  });

  it('autofocuses the path input on mount so the user can type immediately', async () => {
    // The path input is marked data-autofocus so that when Modal's
    // focus-trap runs on open it picks the input as the initial focus
    // target. That attribute must be present on the input.
    const { getByTestId } = render(DirectoryBrowser, {
      props: { initialPath: '~' },
    });
    await flushMount();
    const input = getByTestId('directory-browser-path');
    expect(input.hasAttribute('data-autofocus')).toBe(true);
  });

  it('recovers from a "No matches" state when the user types a valid path again', async () => {
    vi.useFakeTimers();
    setBindingMock('BrowseDirectory', async (path: string) => {
      if (path === '~') return mkListing();
      if (path.startsWith('/Users/me')) return mkListing({ path });
      return mkMissing(path);
    });
    const { getByTestId, queryByTestId, findAllByTestId } = render(DirectoryBrowser, {
      props: { initialPath: '~' },
    });
    await vi.advanceTimersByTimeAsync(0);
    for (let i = 0; i < 3; i += 1) await tick();

    const input = getByTestId('directory-browser-path') as HTMLInputElement;

    // Invalid — wipes listing.
    await fireEvent.input(input, { target: { value: '/nope' } });
    await vi.advanceTimersByTimeAsync(120);
    await Promise.resolve();
    for (let i = 0; i < 4; i += 1) await tick();
    expect(queryByTestId('directory-browser-no-matches')).not.toBeNull();

    // Typing a valid path again clears the hint and shows entries.
    await fireEvent.input(input, { target: { value: '/Users/me' } });
    await vi.advanceTimersByTimeAsync(120);
    await Promise.resolve();
    for (let i = 0; i < 4; i += 1) await tick();

    expect(queryByTestId('directory-browser-no-matches')).toBeNull();
    const entries = await findAllByTestId('directory-browser-entry');
    expect(entries.length).toBeGreaterThan(0);
    vi.useRealTimers();
  });
});
