import { describe, expect, it } from 'vitest';
import { render, fireEvent, waitFor } from '@testing-library/svelte';
import ClaudeCommandTimeoutField from './ClaudeCommandTimeoutField.svelte';
import { loadSettingsFixture as loadSettings } from '../../../test/helpers/settingsFixture';
import { setBindingMock, getBindingMock } from '../../../test/mocks/bindings-app';
import { makeSettings } from '../../../test/helpers/settings';
import type { Settings } from '../../types/settings';

async function seed(overrides: Partial<Settings> = {}): Promise<void> {
  const merged: Settings = { ...makeSettings(), ...overrides };
  setBindingMock('GetSettings', async () => merged);
  setBindingMock('UpdateSettings', async (patch: unknown) => ({
    ...merged,
    ...((patch as Record<string, unknown>) ?? {}),
  }));
  await loadSettings();
}

function patches(): Record<string, unknown>[] {
  return (getBindingMock('UpdateSettings')?.mock.calls ?? []).map(
    (call) => call[0] as Record<string, unknown>,
  );
}

describe('<ClaudeCommandTimeoutField>', () => {
  it('shows the 5 hour default and no override note', async () => {
    await seed();
    const { getByTestId, queryByTestId } = render(ClaudeCommandTimeoutField);
    expect((getByTestId('settings-claude-command-timeout') as HTMLInputElement).value).toBe('5');
    expect(queryByTestId('settings-claude-command-timeout-overridden')).toBeNull();
  });

  it('writes whole hours', async () => {
    await seed();
    const { getByTestId } = render(ClaudeCommandTimeoutField);
    await fireEvent.change(getByTestId('settings-claude-command-timeout'), {
      target: { value: '12' },
    });
    await waitFor(() => expect(patches().at(-1)).toEqual({ claudeMaxCommandTimeoutHours: 12 }));
  });

  it('clamps into the range and repaints the field with what was stored', async () => {
    await seed();
    const { getByTestId } = render(ClaudeCommandTimeoutField);
    const input = getByTestId('settings-claude-command-timeout') as HTMLInputElement;

    await fireEvent.change(input, { target: { value: '1' } });
    await waitFor(() => expect(patches().at(-1)).toEqual({ claudeMaxCommandTimeoutHours: 2 }));
    expect(input.value).toBe('2');

    await fireEvent.change(input, { target: { value: '9999' } });
    await waitFor(() => expect(patches().at(-1)).toEqual({ claudeMaxCommandTimeoutHours: 168 }));
    expect(input.value).toBe('168');
  });

  it('keeps the stored value for an empty entry and writes nothing', async () => {
    await seed({ claudeMaxCommandTimeoutHours: 8 });
    const { getByTestId } = render(ClaudeCommandTimeoutField);
    const input = getByTestId('settings-claude-command-timeout') as HTMLInputElement;
    await fireEvent.change(input, { target: { value: '' } });
    expect(input.value).toBe('8');
    expect(patches()).toHaveLength(0);
  });

  it('says a custom BASH_MAX_TIMEOUT_MS overrides it', async () => {
    await seed({ claudeCustomEnv: [{ name: 'BASH_MAX_TIMEOUT_MS', value: '' }] });
    const { getByTestId } = render(ClaudeCommandTimeoutField);
    expect(getByTestId('settings-claude-command-timeout-overridden').textContent).toContain(
      'BASH_MAX_TIMEOUT_MS in Environment overrides this.',
    );
  });
});
