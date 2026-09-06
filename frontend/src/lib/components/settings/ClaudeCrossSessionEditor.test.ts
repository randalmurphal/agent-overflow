import { describe, expect, it } from 'vitest';
import { render, fireEvent, waitFor } from '@testing-library/svelte';
import ClaudeCrossSessionEditor from './ClaudeCrossSessionEditor.svelte';
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

function lastPatch(): Record<string, unknown> {
  const mock = getBindingMock('UpdateSettings');
  expect(mock).toBeDefined();
  expect(mock!.mock.calls.length).toBeGreaterThan(0);
  return mock!.mock.calls.at(-1)![0] as Record<string, unknown>;
}

describe('<ClaudeCrossSessionEditor>', () => {
  // Off by default and the policy select hidden with it: the policy only
  // means anything once the inbox is bound, and showing it while the
  // feature is off invites the belief that "Ignore" is what turns it off.
  it('is off by default and hides the policy until it is on', async () => {
    await seed();
    const { getByRole, queryByTestId } = render(ClaudeCrossSessionEditor);

    expect(getByRole('switch').getAttribute('aria-checked')).toBe('false');
    expect(queryByTestId('settings-claude-cross-session-inbound')).toBeNull();
  });

  // Enabling must write an explicit policy. An enabled-but-unset session
  // falls into Claude Code's mode-parity hold, which drops peer messages
  // after a timeout with nothing on the wire to say so.
  it('resolves a policy when the feature is switched on', async () => {
    await seed();
    const { getByRole } = render(ClaudeCrossSessionEditor);

    await fireEvent.click(getByRole('switch'));

    await waitFor(() =>
      expect(lastPatch()).toEqual({ claudeCrossSession: { enabled: true, inbound: 'accept' } }),
    );
  });

  it('writes both halves together when the policy changes', async () => {
    await seed({ claudeCrossSession: { enabled: true, inbound: 'accept' } });
    const { getByTestId } = render(ClaudeCrossSessionEditor);

    await fireEvent.change(getByTestId('settings-claude-cross-session-inbound'), {
      target: { value: 'refuse' },
    });

    await waitFor(() =>
      expect(lastPatch()).toEqual({ claudeCrossSession: { enabled: true, inbound: 'refuse' } }),
    );
  });

  // Turning it off keeps the policy so turning it back on finds the user's
  // choice, and never writes `enabled: false` — the sparse writer treats
  // absence as the default.
  it('keeps the stored policy when switched off', async () => {
    await seed({ claudeCrossSession: { enabled: true, inbound: 'refuse' } });
    const { getByRole } = render(ClaudeCrossSessionEditor);

    await fireEvent.click(getByRole('switch'));

    await waitFor(() => expect(lastPatch()).toEqual({ claudeCrossSession: { inbound: 'refuse' } }));
  });

  // The retained policy must SURVIVE the round trip, not just the write that
  // stored it. The editor used to bind the runtime-resolved value, which is
  // deliberately empty while the feature is off — so a stored "refuse" showed
  // as "Accept" and re-enabling wrote that fallback back over it.
  it('re-enables on the retained policy rather than the resolved fallback', async () => {
    await seed({ claudeCrossSession: { inbound: 'refuse' } });
    const { getByRole, getByTestId } = render(ClaudeCrossSessionEditor);

    await fireEvent.click(getByRole('switch'));

    await waitFor(() =>
      expect(lastPatch()).toEqual({ claudeCrossSession: { enabled: true, inbound: 'refuse' } }),
    );
    expect((getByTestId('settings-claude-cross-session-inbound') as HTMLSelectElement).value).toBe(
      'refuse',
    );
  });

  // A setting that has never carried a policy still resolves to one: an
  // enabled-but-unset session falls into Claude Code's mode-parity hold.
  it('falls back to accept only when no policy was ever stored', async () => {
    await seed({ claudeCrossSession: {} });
    const { getByRole } = render(ClaudeCrossSessionEditor);

    await fireEvent.click(getByRole('switch'));

    await waitFor(() =>
      expect(lastPatch()).toEqual({ claudeCrossSession: { enabled: true, inbound: 'accept' } }),
    );
  });

  // Every axis here binds during the CLI's setup and nothing rebinds it, so
  // a save cannot reach a running session without a restart. The notice is
  // the difference between "nothing happened" and "it will".
  it('says that running sessions keep their setting until they restart', async () => {
    await seed();
    const { getByRole, getByTestId } = render(ClaudeCrossSessionEditor);

    await fireEvent.click(getByRole('switch'));

    await waitFor(() =>
      expect(getByTestId('settings-claude-cross-session-deferred').textContent).toContain(
        'until they next restart',
      ),
    );
  });

  // Same notice, same rule as the thinking axis: it is a claim about a save
  // that landed. updateSettingsPatch restores the keys it patched and never
  // rejects, so without the stored-value witness the toggle would snap back
  // to off while the notice below it still promised a pending restart.
  it('drops the restart notice when the save fails', async () => {
    await seed();
    const { getByRole, queryByTestId } = render(ClaudeCrossSessionEditor);
    setBindingMock('UpdateSettings', async () => {
      throw new Error('settings write refused');
    });

    await fireEvent.click(getByRole('switch'));

    await waitFor(() => expect(getByRole('switch').getAttribute('aria-checked')).toBe('false'));
    expect(queryByTestId('settings-claude-cross-session-deferred')).toBeNull();
  });

  it('never offers a policy Agent Overflow refuses to write', async () => {
    await seed({ claudeCrossSession: { enabled: true } });
    const { getByTestId } = render(ClaudeCrossSessionEditor);

    const options = Array.from(
      (getByTestId('settings-claude-cross-session-inbound') as HTMLSelectElement).options,
    ).map((o) => o.value);
    expect(options).toEqual(['accept', 'refuse']);
  });
});
