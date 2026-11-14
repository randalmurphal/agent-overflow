import { describe, expect, it, beforeEach } from 'vitest';
import { render, fireEvent, waitFor } from '@testing-library/svelte';
import StorageSettings from './StorageSettings.svelte';
import { loadSettingsFixture as loadSettings } from '../../../test/helpers/settingsFixture';
import { setBindingMock, getBindingMock } from '../../../test/mocks/bindings-app';
import type { Settings } from '../../types/settings';
import { makeSettings } from '../../../test/helpers/settings';
import { setPageGrantsFromBootstrap } from '../../transport/scopes';
import { HOST_TIER_REASON } from './settingsComputer';

async function seed(overrides: Partial<Settings> = {}): Promise<Settings> {
  const merged = makeSettings(overrides);
  setBindingMock('GetSettings', async () => merged);
  setBindingMock('UpdateSettings', async (patch: unknown) => {
    const p = (patch as Record<string, unknown>) ?? {};
    return { ...merged, ...p };
  });
  setBindingMock('ListArchivedThreads', async () => []);
  setBindingMock('GetClaudeTranscriptRetention', async (days: number) => claudeRetention(days));
  await loadSettings();
  return merged;
}

// What the backend answers for a Claude home that sets no cleanupPeriodDays:
// its 30-day default stands until the window is longer, and a disabled
// cleanup asks for the cap.
function claudeRetention(days: number) {
  const keep = days === 0 ? 36500 : days;
  return {
    settingsPath: '/home/test/.claude/settings.json',
    claudeDays: 30,
    explicit: false,
    sessionDays: keep > 30 ? keep : 0,
  };
}

describe('<StorageSettings> — Retention', () => {
  beforeEach(async () => {
    await seed();
  });

  it('renders the retention input with the default value', async () => {
    const { getByTestId } = render(StorageSettings);
    expect(getByTestId('settings-retention')).toBeTruthy();
    const input = getByTestId('settings-retention-days') as HTMLInputElement;
    expect(input.value).toBe('30');
  });

  it('renders the retention input inert, saying why, off the host without a passkey', async () => {
    // `retention` is host tier (internal/settings/tier.go): the backend takes
    // the write only under a step-up proof, which a networked page with no
    // host presence and no passkey cannot give.
    setPageGrantsFromBootstrap(true);
    try {
      const { getByTestId } = render(StorageSettings);
      const input = getByTestId('settings-retention-days') as HTMLInputElement;
      expect(input.disabled).toBe(true);
      expect(input.title).toBe(HOST_TIER_REASON);
    } finally {
      setPageGrantsFromBootstrap(false);
    }
  });

  it('shows the disabled-cleanup hint when retention days is 0', async () => {
    await seed({ retention: { days: 0 } });
    const { getByText } = render(StorageSettings);
    expect(getByText(/automatic cleanup is disabled/i)).toBeTruthy();
  });

  it('dispatches retention patch on blur with parsed integer', async () => {
    const { getByTestId } = render(StorageSettings);
    const input = getByTestId('settings-retention-days') as HTMLInputElement;
    input.value = '7';
    await fireEvent.blur(input);

    const mock = getBindingMock('UpdateSettings');
    expect(mock).toBeDefined();
    expect(mock!.mock.calls[0][0]).toEqual({ retention: { days: 7 } });
  });

  it('coerces non-numeric input to 0 (disabled)', async () => {
    const { getByTestId } = render(StorageSettings);
    const input = getByTestId('settings-retention-days') as HTMLInputElement;
    input.value = 'banana';
    await fireEvent.blur(input);

    const mock = getBindingMock('UpdateSettings');
    expect(mock).toBeDefined();
    expect(mock!.mock.calls[0][0]).toEqual({ retention: { days: 0 } });
  });

  it('coerces negative input to 0 (disabled)', async () => {
    const { getByTestId } = render(StorageSettings);
    const input = getByTestId('settings-retention-days') as HTMLInputElement;
    input.value = '-5';
    await fireEvent.blur(input);

    const mock = getBindingMock('UpdateSettings');
    expect(mock).toBeDefined();
    expect(mock!.mock.calls[0][0]).toEqual({ retention: { days: 0 } });
  });

  it('clamps above the Go-side retention cap', async () => {
    const { getByTestId } = render(StorageSettings);
    const input = getByTestId('settings-retention-days') as HTMLInputElement;
    input.value = '99999';
    await fireEvent.blur(input);

    const mock = getBindingMock('UpdateSettings');
    expect(mock!.mock.calls[0][0]).toEqual({ retention: { days: 36500 } });
  });
});

describe('<StorageSettings> — Claude transcript retention note', () => {
  it('says nothing while the window is no longer than Claude Code keeps', async () => {
    await seed();
    const { queryByTestId } = render(StorageSettings);
    await waitFor(() => expect(getBindingMock('GetClaudeTranscriptRetention')).toHaveBeenCalledWith(30));
    expect(queryByTestId('settings-claude-retention-note')).toBeNull();
    expect(queryByTestId('settings-claude-retention-error')).toBeNull();
  });

  it('names the setting to change when the window is longer', async () => {
    await seed({ retention: { days: 90 } });
    const { findByTestId } = render(StorageSettings);
    const note = await findByTestId('settings-claude-retention-note');
    const text = note.textContent?.replace(/\s+/g, ' ') ?? '';
    expect(text).toContain('after 30 days by default');
    expect(text).toContain('keep them for 90 days');
    expect(text).toContain('"cleanupPeriodDays": 90');
    expect(text).toContain('/home/test/.claude/settings.json');
    // A workspace's own Claude settings outrank the user file, so one edit
    // there is not presented as the whole fix.
    expect(text).toContain('raise any shorter value a workspace sets');
    expect(text).toContain('.claude/settings.local.json');
  });

  it('asks again with the value the page shows, and withdraws the note when it no longer applies', async () => {
    await seed({ retention: { days: 90 } });
    const { findByTestId, getByTestId, queryByTestId } = render(StorageSettings);
    await findByTestId('settings-claude-retention-note');

    const input = getByTestId('settings-retention-days') as HTMLInputElement;
    input.value = '14';
    await fireEvent.blur(input);

    await waitFor(() => expect(queryByTestId('settings-claude-retention-note')).toBeNull());
    expect(getBindingMock('GetClaudeTranscriptRetention')).toHaveBeenLastCalledWith(14);

    input.value = '0';
    await fireEvent.blur(input);
    const note = await findByTestId('settings-claude-retention-note');
    expect(note.textContent).toContain('"cleanupPeriodDays": 36500');
  });

  it('withdraws the note at once when the value changes, before the new answer arrives', async () => {
    await seed({ retention: { days: 90 } });
    const { findByTestId, getByTestId, queryByTestId } = render(StorageSettings);
    await findByTestId('settings-claude-retention-note');

    // The answer for the new value is held, as a slow backend would hold it.
    let release!: () => void;
    const held = new Promise<void>((resolve) => {
      release = resolve;
    });
    setBindingMock('GetClaudeTranscriptRetention', async (days: number) => {
      await held;
      return claudeRetention(days);
    });

    const input = getByTestId('settings-retention-days') as HTMLInputElement;
    input.value = '180';
    await fireEvent.blur(input);
    await waitFor(() => expect(getBindingMock('GetClaudeTranscriptRetention')).toHaveBeenCalledWith(180));
    expect(queryByTestId('settings-claude-retention-note')).toBeNull();

    release();
    const note = await findByTestId('settings-claude-retention-note');
    expect(note.textContent).toContain('"cleanupPeriodDays": 180');
  });

  it('drops the by-default wording when the Claude settings file sets a period', async () => {
    await seed({ retention: { days: 90 } });
    setBindingMock('GetClaudeTranscriptRetention', async () => ({
      settingsPath: '/home/test/.claude/settings.json',
      claudeDays: 7,
      explicit: true,
      sessionDays: 90,
    }));
    const { findByTestId } = render(StorageSettings);
    const text = (await findByTestId('settings-claude-retention-note')).textContent ?? '';
    expect(text).toContain('after 7 days.');
    expect(text).not.toContain('by default');
  });

  it("shows why Claude Code's own setting could not be read", async () => {
    await seed({ retention: { days: 90 } });
    setBindingMock('GetClaudeTranscriptRetention', async () => ({
      settingsPath: '/home/test/.claude/settings.json',
      claudeDays: 0,
      explicit: false,
      sessionDays: 0,
      error: 'parse Claude settings /home/test/.claude/settings.json: unexpected end of JSON input',
    }));
    const { findByTestId, queryByTestId } = render(StorageSettings);
    const error = await findByTestId('settings-claude-retention-error');
    expect(error.textContent).toContain('unexpected end of JSON input');
    expect(queryByTestId('settings-claude-retention-note')).toBeNull();
  });

  it('shows a failed check instead of dropping it', async () => {
    await seed({ retention: { days: 90 } });
    setBindingMock('GetClaudeTranscriptRetention', async () => {
      throw new Error('backend unavailable');
    });
    const { findByTestId } = render(StorageSettings);
    const error = await findByTestId('settings-claude-retention-error');
    expect(error.textContent).toContain('backend unavailable');
  });
});

describe('<StorageSettings> — Archived threads', () => {
  beforeEach(async () => {
    await seed();
  });

  it('mounts the archive list under the retention section', async () => {
    const { findByTestId } = render(StorageSettings);
    expect(await findByTestId('settings-archived-threads')).toBeTruthy();
  });
});
