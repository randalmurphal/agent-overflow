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
    setBindingMock('GetKeybindings', async () => initialRules);

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
    setBindingMock('GetKeybindings', async () => THREAD_NEW_ROWS);

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
    setBindingMock('GetKeybindings', async () => THREAD_NEW_ROWS);

    const { findByRole, getByRole } = render(KeybindingsSettings);
    await findByRole('button', { name: 'Ctrl+N' });

    await fireEvent.click(getByRole('button', { name: 'Ctrl+N' }));
    const capture = getByRole('button', { name: 'Press keys... (Esc to cancel)' });
    await fireEvent.keyDown(capture, { key: 'n', ctrlKey: true });

    expect(update).toHaveBeenCalledTimes(1);
    await waitFor(() => expect(getToasts().map((t) => t.type)).toEqual(['success']));
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
      setBindingMock('GetKeybindings', async () => ROWS);

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
    setBindingMock('GetKeybindings', async () => ROWS);

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
