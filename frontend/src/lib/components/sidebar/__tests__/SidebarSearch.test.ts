import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
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
import { resetKeyboardModifiersForTest } from '../../../stores/keyboardModifiers.svelte';

const FOCUS_SEARCH_ROW = {
  key: 'mod+/',
  command: 'sidebar.focus-search',
  defaultId: 'sidebar.focus-search',
  defaultKey: 'mod+/',
};

/** Hold the modifier past the jump-hint delay, the way the thread rows'
 * jump pills are revealed: the chord pill shares that door. */
async function holdModifier(): Promise<void> {
  window.dispatchEvent(new KeyboardEvent('keydown', { key: 'Control', bubbles: true }));
  vi.advanceTimersByTime(101);
  await tick();
}

describe('<SidebarSearch>', () => {
  beforeEach(() => {
    vi.useFakeTimers();
    resetKeyboardModifiersForTest();
    setThreadFilterQuery('');
    resetKeybindingsStore();
    setKeybindingsForTest([FOCUS_SEARCH_ROW]);
  });

  afterEach(() => {
    resetKeyboardModifiersForTest();
    vi.useRealTimers();
  });

  it('renders the search input with no keybind affordance at rest', () => {
    // A chord pill at rest is chrome the phone can never act on and the
    // desktop already knows; it appears while the modifier is held, the
    // same moment the thread rows show their jump numbers.
    const { getByTestId, queryByTestId } = render(SidebarSearch, {
      props: {},
    });
    expect(getByTestId('sidebar-thread-search')).toBeInTheDocument();
    expect(queryByTestId('sidebar-thread-search-kbd')).toBeNull();
    expect(queryByTestId('sidebar-thread-search-clear')).toBeNull();
  });

  it('shows the keybind affordance while the modifier is held, and drops it on release', async () => {
    const { queryByTestId } = render(SidebarSearch, { props: {} });
    await holdModifier();
    expect(queryByTestId('sidebar-thread-search-kbd')).toBeInTheDocument();
    window.dispatchEvent(new KeyboardEvent('keyup', { key: 'Control', bubbles: true }));
    await tick();
    expect(queryByTestId('sidebar-thread-search-kbd')).toBeNull();
  });

  it('keybind affordance reflects the configured sidebar.focus-search chord', async () => {
    // Regression guard for the hardcoded-chord bug: the pill must read the
    // live binding. Whether the displayed text is `Ctrl+/` or `⌘/` depends
    // on platform, but `/` is invariant — and the broken state (a hardcoded
    // chord) would render whatever was hardcoded instead.
    setKeybindingsForTest([{ ...FOCUS_SEARCH_ROW, key: 'mod+/' }]);
    const { getByTestId } = render(SidebarSearch, { props: {} });
    await holdModifier();
    const text = getByTestId('sidebar-thread-search-kbd').textContent ?? '';
    expect(text).toContain('/');
    expect(text).not.toContain('K');
  });

  it('re-reads the chord after a rebind', async () => {
    setKeybindingsForTest([{ ...FOCUS_SEARCH_ROW, key: 'mod+shift+j' }]);
    const { getByTestId } = render(SidebarSearch, { props: {} });
    await holdModifier();
    expect(getByTestId('sidebar-thread-search-kbd').textContent ?? '').toContain('J');
  });

  it('drops the affordance entirely when sidebar.focus-search is unbound', async () => {
    // The hint promises a keystroke. With the command cleared there is no
    // keystroke to promise, and the old `?? 'mod+/'` fallback would have
    // shown the very chord the user removed.
    setKeybindingsForTest([{ ...FOCUS_SEARCH_ROW, key: UNBOUND_CHORD }]);
    const { queryByTestId } = render(SidebarSearch, { props: {} });
    await holdModifier();
    expect(queryByTestId('sidebar-thread-search-kbd')).toBeNull();
  });

  it('swaps the keybind affordance for a clear button once the input has content', async () => {
    const { getByTestId, queryByTestId } = render(SidebarSearch, {
      props: {},
    });
    await holdModifier();
    const input = getByTestId('sidebar-thread-search') as HTMLInputElement;
    await fireEvent.input(input, { target: { value: 'search text' } });
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
