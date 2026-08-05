import { describe, expect, it, beforeEach, vi } from 'vitest';
import { render, fireEvent, waitFor } from '@testing-library/svelte';
import KeybindingsSettings from './KeybindingsSettings.svelte';
import { dispatchKey, resetKeybindingsStore } from '../../stores/keybindings.svelte';
import {
  clearCommandRegistry,
  registerCommand,
  type CommandContext,
} from '../../stores/commandRegistry.svelte';
import {
  isSidebarCollapsed,
  resetSidebarLayoutForTest,
  toggleSidebarCollapsed,
} from '../../stores/sidebarLayout.svelte';
import { resetAppStorageForTest } from '../../stores/appStorage';
import { getToasts, removeToast } from '../../stores/toast.svelte';
import { setBindingMock } from '../../../test/mocks/bindings-app';

const THREAD_NEW_ROWS = [
  {
    key: 'mod+n',
    command: 'thread.new',
    when: '!terminalFocus',
    defaultId: 'thread.new.primary',
    defaultKey: 'mod+n',
  },
  {
    key: 'mod+shift+o',
    command: 'thread.new',
    when: '!terminalFocus',
    defaultId: 'thread.new.alternate',
    defaultKey: 'mod+shift+o',
  },
];

describe('<KeybindingsSettings>', () => {
  beforeEach(() => {
    resetKeybindingsStore();
    for (const t of [...getToasts()]) removeToast(t.id);
  });

  it('rebinds duplicate command/context rows by default identity', async () => {
    const initialRules = THREAD_NEW_ROWS;
    const update = setBindingMock('UpdateKeybindings', vi.fn(async () => {}));
    setBindingMock('GetKeybindings', async () => ({ bindings: initialRules }));

    const { findByRole, getByRole } = render(KeybindingsSettings);
    await findByRole('button', { name: 'Ctrl+N' });

    await fireEvent.click(getByRole('button', { name: 'Ctrl+Shift+O' }));
    const capture = getByRole('button', { name: 'Press keys... (Esc to cancel)' });
    await fireEvent.keyDown(capture, { key: 'x', ctrlKey: true });

    expect(update).toHaveBeenCalledTimes(1);
    expect(update.mock.calls[0]?.[0]).toMatchObject([
      {
        key: 'mod+x',
        command: 'thread.new',
        when: '!terminalFocus',
        defaultId: 'thread.new.alternate',
        defaultKey: 'mod+shift+o',
      },
    ]);
  });

  it('refuses a rebind onto a sibling row of the same command and context', async () => {
    // thread.new ships two default rows. The override model holds one slot per
    // default identity, so moving the primary row onto the alternate's chord
    // could only persist a second row displaying the same chord for the same
    // command — invisible to tell apart, and unreachable past the first match.
    const update = setBindingMock('UpdateKeybindings', vi.fn(async () => {}));
    setBindingMock('GetKeybindings', async () => ({ bindings: THREAD_NEW_ROWS }));

    const { findByRole, getByRole } = render(KeybindingsSettings);
    await findByRole('button', { name: 'Ctrl+N' });

    await fireEvent.click(getByRole('button', { name: 'Ctrl+N' }));
    const capture = getByRole('button', { name: 'Press keys... (Esc to cancel)' });
    await fireEvent.keyDown(capture, { key: 'O', ctrlKey: true, shiftKey: true });

    expect(update).not.toHaveBeenCalled();
    expect(getToasts().map((t) => [t.type, t.message])).toEqual([
      ['error', 'Ctrl+Shift+O is already bound to thread.new in this context — change that shortcut first'],
    ]);
    // The two rows still display distinct chords: no (command, when, chord)
    // pair repeats anywhere in the table.
    const chords = ['Ctrl+N', 'Ctrl+Shift+O'].map((name) => getByRole('button', { name }));
    expect(new Set(chords).size).toBe(2);
  });

  it('allows re-capturing a row onto the chord it already has', async () => {
    // Same-identity capture is a no-op rebind, not a collision.
    const update = setBindingMock('UpdateKeybindings', vi.fn(async () => {}));
    setBindingMock('GetKeybindings', async () => ({ bindings: THREAD_NEW_ROWS }));

    const { findByRole, getByRole } = render(KeybindingsSettings);
    await findByRole('button', { name: 'Ctrl+N' });

    await fireEvent.click(getByRole('button', { name: 'Ctrl+N' }));
    const capture = getByRole('button', { name: 'Press keys... (Esc to cancel)' });
    await fireEvent.keyDown(capture, { key: 'n', ctrlKey: true });

    expect(update).toHaveBeenCalledTimes(1);
    await waitFor(() => expect(getToasts().map((t) => t.type)).toEqual(['success']));
  });
});

// --- the unreadable-file warning ---
//
// Saving any row here REPLACES the user's file. When that file could not be
// read, the table is showing defaults, so an edit silently destroys overrides
// the user still has on disk. The banner is the informed consent for that.

describe('<KeybindingsSettings> unreadable keybindings file', () => {
  const LOAD_ERROR =
    "parse keybindings /home/u/.config/agent-overflow/keybindings.json: invalid character 'n'";

  beforeEach(() => {
    resetKeybindingsStore();
    for (const t of [...getToasts()]) removeToast(t.id);
  });

  it('warns with the backend reason and says a save replaces the file', async () => {
    setBindingMock('GetKeybindings', async () => ({
      bindings: THREAD_NEW_ROWS,
      loadError: LOAD_ERROR,
    }));

    const { findByRole, getByRole } = render(KeybindingsSettings);

    const banner = await findByRole('alert');
    const text = banner.textContent ?? '';
    expect(text).toContain('could not be read');
    // The path is in the reason, and the reason is what tells the user which
    // file to rescue before touching anything here.
    expect(text).toContain(LOAD_ERROR);
    expect(text).toContain('replaces');
    // The defaults are still editable underneath — the warning is not a
    // blocked state.
    expect(getByRole('button', { name: 'Ctrl+N' })).toBeInTheDocument();
  });

  it('renders no warning when the file loaded cleanly', async () => {
    setBindingMock('GetKeybindings', async () => ({ bindings: THREAD_NEW_ROWS }));

    const { findByRole, queryByRole } = render(KeybindingsSettings);
    await findByRole('button', { name: 'Ctrl+N' });
    expect(queryByRole('alert')).toBeNull();
  });
});

// --- clearing a binding to "unbound", and getting back out of it ---
//
// "Absent" already meant "use the default", so before the sentinel existed a
// user could not remove a default chord at all. These cover the round trip in
// the surface that owns it, including the transition back to the default.

describe('<KeybindingsSettings> unbound bindings', () => {
  /**
   * A stand-in for keybindings.Merge: shipped defaults with the stored user
   * overrides layered on by defaultId. Lets a test drive several edits in a
   * row and see each one through a real save + reload.
   */
  function mockBackend(defaults: typeof THREAD_NEW_ROWS) {
    let stored: Array<Record<string, unknown>> = [];
    const update = setBindingMock('UpdateKeybindings', vi.fn(async (payload) => {
      stored = (payload as Array<Record<string, unknown>>).map((r) => ({ ...r }));
    }));
    setBindingMock('GetKeybindings', async () => ({
      bindings: defaults.map((d) => {
        const override = stored.find((r) => r.defaultId === d.defaultId);
        return override ? { ...d, ...override, defaultKey: d.defaultKey } : d;
      }),
    }));
    return { update, stored: () => stored };
  }

  /**
   * Click a control in the table, waiting for it to be enabled first.
   *
   * The table disables its controls while a save is in flight, and the row
   * re-renders with the saved value one flush BEFORE `saving` clears — so the
   * new chord is on screen while the buttons are still disabled. happy-dom
   * drops a click on a disabled button silently (returns false without
   * dispatching), which would make a follow-up interaction a no-op that reads
   * as a product bug. Waiting is the fix; the `!== false` assertion is the
   * tripwire so a dropped click can never pass quietly.
   */
  async function click(get: () => HTMLElement): Promise<void> {
    await waitFor(() => expect((get() as HTMLButtonElement).disabled).toBe(false));
    expect(await fireEvent.click(get())).not.toBe(false);
  }

  beforeEach(() => {
    resetKeybindingsStore();
    for (const t of [...getToasts()]) removeToast(t.id);
  });

  it('clears a row to unbound and persists the sentinel with its identity', async () => {
    const { update } = mockBackend(THREAD_NEW_ROWS);

    const { findByRole, getByRole } = render(KeybindingsSettings);
    await findByRole('button', { name: 'Ctrl+N' });

    await click(() => getByRole('button', { name: 'Clear the Ctrl+N shortcut for thread.new' }));

    expect(update).toHaveBeenCalledTimes(1);
    expect(update.mock.calls[0]?.[0]).toMatchObject([
      { key: '', command: 'thread.new', defaultId: 'thread.new.primary', defaultKey: 'mod+n' },
    ]);
    // The chord is gone from the table, and the row is still there.
    await waitFor(() => expect(getByRole('button', { name: 'Unbound' })).toBeInTheDocument());
    expect(getByRole('button', { name: 'Ctrl+Shift+O' })).toBeInTheDocument();
  });

  it('restores the default after a clear, dropping the override entirely', async () => {
    const backend = mockBackend(THREAD_NEW_ROWS);

    const { findByRole, getByRole, queryByRole } = render(KeybindingsSettings);
    await findByRole('button', { name: 'Ctrl+N' });

    await click(() => getByRole('button', { name: 'Clear the Ctrl+N shortcut for thread.new' }));
    await waitFor(() => expect(getByRole('button', { name: 'Unbound' })).toBeInTheDocument());

    await click(() =>
      getByRole('button', { name: 'Restore thread.new to its default shortcut Ctrl+N' }));

    // Restoring writes no override at all — absence IS the default, so the
    // user file must not pin today's chord against a future change to it.
    await waitFor(() => expect(backend.stored()).toEqual([]));
    await waitFor(() => expect(getByRole('button', { name: 'Ctrl+N' })).toBeInTheDocument());
    expect(queryByRole('button', { name: 'Unbound' })).toBeNull();
  });

  it('survives rebind → clear → rebind on the same row', async () => {
    const backend = mockBackend(THREAD_NEW_ROWS);

    const { findByRole, getByRole } = render(KeybindingsSettings);
    await findByRole('button', { name: 'Ctrl+N' });

    async function capture(from: string, key: string, mods: Record<string, boolean>): Promise<void> {
      await click(() => getByRole('button', { name: from }));
      await fireEvent.keyDown(
        getByRole('button', { name: 'Press keys... (Esc to cancel)' }),
        { key, ...mods },
      );
    }

    await capture('Ctrl+N', 'x', { ctrlKey: true });
    await waitFor(() => expect(getByRole('button', { name: 'Ctrl+X' })).toBeInTheDocument());

    await click(() => getByRole('button', { name: 'Clear the Ctrl+X shortcut for thread.new' }));
    await waitFor(() => expect(getByRole('button', { name: 'Unbound' })).toBeInTheDocument());
    // The clear must overwrite the previous rebind, not sit beside it.
    expect(backend.stored()).toMatchObject([{ key: '', defaultId: 'thread.new.primary' }]);

    // ...and a rebind must lift the row back out of unbound.
    await capture('Unbound', 'y', { ctrlKey: true });
    await waitFor(() => expect(getByRole('button', { name: 'Ctrl+Y' })).toBeInTheDocument());
    expect(backend.stored()).toMatchObject([{ key: 'mod+y', defaultId: 'thread.new.primary' }]);
  });

  it('distinguishes default, custom and unbound chords visually', async () => {
    mockBackend(THREAD_NEW_ROWS);

    const { findByRole, getByRole, getByTestId } = render(KeybindingsSettings);
    await findByRole('button', { name: 'Ctrl+N' });

    const chordState = (defaultId: string): string | null =>
      getByTestId(`keybinding-row-${defaultId}`)
        .querySelector('[data-chord-state]')
        ?.getAttribute('data-chord-state') ?? null;

    expect(chordState('thread.new.primary')).toBe('default');

    await click(() => getByRole('button', { name: 'Ctrl+N' }));
    await fireEvent.keyDown(
      getByRole('button', { name: 'Press keys... (Esc to cancel)' }),
      { key: 'x', ctrlKey: true },
    );
    await waitFor(() => expect(chordState('thread.new.primary')).toBe('custom'));

    await click(() => getByRole('button', { name: 'Clear the Ctrl+X shortcut for thread.new' }));
    await waitFor(() => expect(chordState('thread.new.primary')).toBe('unbound'));
    // Only the edited row changes appearance.
    expect(chordState('thread.new.alternate')).toBe('default');
  });

  it('clearing a second row is not treated as a collision with the first', async () => {
    // Both rows share a command and context. Rebinding one onto the other's
    // chord is refused, but CLEARING both is legitimate — neither cleared row
    // is reachable, so they cannot shadow each other.
    const backend = mockBackend(THREAD_NEW_ROWS);

    const { findByRole, getByRole } = render(KeybindingsSettings);
    await findByRole('button', { name: 'Ctrl+N' });

    await click(() => getByRole('button', { name: 'Clear the Ctrl+N shortcut for thread.new' }));
    await waitFor(() => expect(backend.stored()).toHaveLength(1));
    await click(() =>
      getByRole('button', { name: 'Clear the Ctrl+Shift+O shortcut for thread.new' }));

    await waitFor(() => expect(backend.stored()).toHaveLength(2));
    expect(backend.stored().map((r) => r.key)).toEqual(['', '']);
    expect(getToasts().every((t) => t.type === 'info')).toBe(true);
  });
});

// --- chord recording must not fire the chord (t3 bug dfbda8436) ---
//
// While this table is capturing, a keystroke is the chord being
// recorded. `sidebar.toggle` (mod+b) is the sharpest case: without the
// capture marker, recording mod+b would rebind AND collapse the
// sidebar out from under the settings surface.

describe('<KeybindingsSettings> chord capture vs. global dispatch', () => {
  // The shipped mod+b row makes the hazard real — dispatch has a live
  // command on that chord — while the palette row is the one the user
  // is recording onto, so the rebind produces an observable override.
  const ROWS = [
    {
      key: 'mod+b',
      command: 'sidebar.toggle',
      defaultId: 'sidebar.toggle',
      defaultKey: 'mod+b',
    },
    {
      key: 'mod+shift+k',
      command: 'palette.open',
      defaultId: 'palette.open',
      defaultKey: 'mod+shift+k',
    },
  ];

  function globalListener(phase: 'capture' | 'bubble'): () => void {
    const ctx = { flags: {} } as unknown as CommandContext;
    const listener = (e: Event) => {
      dispatchKey(e as KeyboardEvent, ctx, { isMac: false });
    };
    window.addEventListener('keydown', listener, phase === 'capture');
    return () => window.removeEventListener('keydown', listener, phase === 'capture');
  }

  beforeEach(() => {
    resetKeybindingsStore();
    clearCommandRegistry();
    resetAppStorageForTest();
    resetSidebarLayoutForTest();
    for (const t of [...getToasts()]) removeToast(t.id);
    setBindingMock('SetUIState', async () => null);
    setBindingMock('DeleteUIState', async () => null);
    registerCommand({
      id: 'sidebar.toggle',
      label: 'Sidebar: Toggle',
      editableReachable: true,
      run: () => toggleSidebarCollapsed(),
    });
  });

  // `capture` models any window listener that sees the event BEFORE the
  // recorder's stopPropagation can stop it — i.e. the guard on its own,
  // with the recorder's caller discipline taken away.
  // `bubble` is App.svelte's actual wiring.
  for (const phase of ['capture', 'bubble'] as const) {
    it(`records mod+b instead of collapsing the sidebar (${phase}-phase listener)`, async () => {
      const update = setBindingMock('UpdateKeybindings', vi.fn(async () => {}));
      setBindingMock('GetKeybindings', async () => ({ bindings: ROWS }));

      const { findByRole, getByRole } = render(KeybindingsSettings);
      await findByRole('button', { name: 'Ctrl+B' });

      await fireEvent.click(getByRole('button', { name: 'Ctrl+Shift+K' }));
      const capture = getByRole('button', { name: 'Press keys... (Esc to cancel)' });

      const detach = globalListener(phase);
      try {
        await fireEvent.keyDown(capture, { key: 'b', ctrlKey: true });
      } finally {
        detach();
      }

      expect(isSidebarCollapsed()).toBe(false);
      expect(update).toHaveBeenCalledTimes(1);
      expect(update.mock.calls[0]?.[0]).toMatchObject([
        { key: 'mod+b', command: 'palette.open', defaultId: 'palette.open' },
      ]);
    });
  }

  it('still collapses when the same chord arrives from outside the recorder', async () => {
    setBindingMock('UpdateKeybindings', vi.fn(async () => {}));
    setBindingMock('GetKeybindings', async () => ({ bindings: ROWS }));

    const { findByRole } = render(KeybindingsSettings);
    await findByRole('button', { name: 'Ctrl+B' });

    const detach = globalListener('bubble');
    try {
      await fireEvent.keyDown(document.body, { key: 'b', ctrlKey: true });
    } finally {
      detach();
    }

    expect(isSidebarCollapsed()).toBe(true);
  });
});
