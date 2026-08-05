import { beforeEach, describe, expect, it } from 'vitest';
import { render, fireEvent } from '@testing-library/svelte';
import { tick } from 'svelte';
import SidebarSearch from '../SidebarSearch.svelte';
import {
  getThreadFilterQuery,
  setThreadFilterQuery,
} from '../../../stores/threadFilter.svelte';
import {
  resetKeybindingsStore,
  setKeybindingsForTest,
  UNBOUND_CHORD,
} from '../../../stores/keybindings.svelte';

const FOCUS_SEARCH_ROW = {
  key: 'mod+/',
  command: 'sidebar.focus-search',
  defaultId: 'sidebar.focus-search',
  defaultKey: 'mod+/',
};

describe('<SidebarSearch>', () => {
  beforeEach(() => {
    setThreadFilterQuery('');
    resetKeybindingsStore();
    setKeybindingsForTest([FOCUS_SEARCH_ROW]);
  });

  it('renders the search input with a keybind affordance when empty', () => {
    const { getByTestId, queryByTestId } = render(SidebarSearch, {
      props: {},
    });
    expect(getByTestId('sidebar-thread-search')).toBeInTheDocument();
    expect(getByTestId('sidebar-thread-search-kbd')).toBeInTheDocument();
    expect(queryByTestId('sidebar-thread-search-clear')).toBeNull();
  });

  it('keybind affordance reflects the configured sidebar.focus-search chord', () => {
    // Regression guard for the hardcoded-chord bug: the pill must read the
    // live binding. Whether the displayed text is `Ctrl+/` or `⌘/` depends
    // on platform, but `/` is invariant — and the broken state (a hardcoded
    // chord) would render whatever was hardcoded instead.
    setKeybindingsForTest([{ ...FOCUS_SEARCH_ROW, key: 'mod+/' }]);
    const { getByTestId } = render(SidebarSearch, { props: {} });
    const text = getByTestId('sidebar-thread-search-kbd').textContent ?? '';
    expect(text).toContain('/');
    expect(text).not.toContain('K');
  });

  it('re-reads the chord after a rebind', () => {
    setKeybindingsForTest([{ ...FOCUS_SEARCH_ROW, key: 'mod+shift+j' }]);
    const { getByTestId } = render(SidebarSearch, { props: {} });
    expect(getByTestId('sidebar-thread-search-kbd').textContent ?? '').toContain('J');
  });

  it('drops the affordance entirely when sidebar.focus-search is unbound', () => {
    // The hint promises a keystroke. With the command cleared there is no
    // keystroke to promise, and the old `?? 'mod+/'` fallback would have
    // shown the very chord the user removed.
    setKeybindingsForTest([{ ...FOCUS_SEARCH_ROW, key: UNBOUND_CHORD }]);
    const { queryByTestId } = render(SidebarSearch, { props: {} });
    expect(queryByTestId('sidebar-thread-search-kbd')).toBeNull();
  });

  it('swaps the keybind affordance for a clear button once the input has content', async () => {
    const { getByTestId, queryByTestId } = render(SidebarSearch, {
      props: {},
    });
    const input = getByTestId('sidebar-thread-search') as HTMLInputElement;
    await fireEvent.input(input, { target: { value: 'design' } });
    await tick();
    expect(queryByTestId('sidebar-thread-search-kbd')).toBeNull();
    expect(getByTestId('sidebar-thread-search-clear')).toBeInTheDocument();
  });

  it('typing drives the shared threadFilter store', async () => {
    const { getByTestId } = render(SidebarSearch, { props: {} });
    const input = getByTestId('sidebar-thread-search') as HTMLInputElement;
    await fireEvent.input(input, { target: { value: 'bug' } });
    expect(getThreadFilterQuery()).toBe('bug');
  });

  it('clear button resets the store and refocuses the input', async () => {
    setThreadFilterQuery('refactor');
    const { getByTestId } = render(SidebarSearch, { props: {} });
    const clearBtn = getByTestId('sidebar-thread-search-clear') as HTMLButtonElement;
    await fireEvent.click(clearBtn);
    expect(getThreadFilterQuery()).toBe('');
  });

  it('exposes a focus function via registerFocusSearch', async () => {
    let focuser: (() => void) | null = null;
    render(SidebarSearch, {
      props: {
        registerFocusSearch: (fn: () => void) => {
          focuser = fn;
        },
      },
    });
    expect(typeof focuser).toBe('function');
  });
});
