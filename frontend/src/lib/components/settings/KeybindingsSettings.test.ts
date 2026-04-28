import { describe, expect, it, beforeEach, vi } from 'vitest';
import { render, fireEvent } from '@testing-library/svelte';
import KeybindingsSettings from './KeybindingsSettings.svelte';
import { resetKeybindingsStore } from '../../stores/keybindings.svelte';
import { setBindingMock } from '../../../test/mocks/bindings-app';

describe('<KeybindingsSettings>', () => {
  beforeEach(() => {
    resetKeybindingsStore();
  });

  it('rebinds duplicate command/context rows by default identity', async () => {
    const initialRules = [
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
});
