import { describe, expect, it, beforeEach } from 'vitest';
import { render, fireEvent } from '@testing-library/svelte';
import ChatSettings from './ChatSettings.svelte';
import { loadSettings } from '../../stores/settings.svelte';
import { setBindingMock, getBindingMock } from '../../../test/mocks/bindings-app';
import type { Settings } from '../../types/settings';
import { makeSettings } from '../../../test/helpers/settings';

async function seed(overrides: Partial<Settings> = {}): Promise<Settings> {
  const merged = makeSettings(overrides);
  setBindingMock('GetSettings', async () => merged);
  setBindingMock('UpdateSettings', async (patch: unknown) => {
    const p = (patch as Record<string, unknown>) ?? {};
    return { ...merged, ...p };
  });
  await loadSettings();
  return merged;
}

describe('<ChatSettings> — Messages', () => {
  beforeEach(async () => {
    await seed();
  });

  it('dispatches collapseDiffPreviews patch when the toggle is clicked', async () => {
    const { getByRole } = render(ChatSettings);
    const toggle = getByRole('switch', { name: 'Toggle Collapse Diff Previews' });
    // On by default, so the first click is the turn-off.
    expect(toggle.getAttribute('aria-checked')).toBe('true');

    await fireEvent.click(toggle);

    const mock = getBindingMock('UpdateSettings');
    expect(mock).toBeDefined();
    expect(mock!.mock.calls[0][0]).toEqual({ collapseDiffPreviews: false });
  });

  it('renders the collapse toggle unchecked when the setting is off', async () => {
    await seed({ collapseDiffPreviews: false });
    const { getByRole } = render(ChatSettings);
    const toggle = getByRole('switch', { name: 'Toggle Collapse Diff Previews' });
    expect(toggle.getAttribute('aria-checked')).toBe('false');
  });
});

describe('<ChatSettings> — Pane density', () => {
  beforeEach(async () => {
    await seed();
  });

  it('dispatches paneDensity patch on change', async () => {
    const { getByTestId } = render(ChatSettings);
    const option = getByTestId('pane-density-option-spacious');
    const input = option.querySelector('input[type="radio"]') as HTMLInputElement;
    await fireEvent.click(input);

    const mock = getBindingMock('UpdateSettings');
    expect(mock).toBeDefined();
    expect(mock!.mock.calls[0][0]).toEqual({ paneDensity: 'spacious' });
  });
});

describe('<ChatSettings> — Activity runs', () => {
  beforeEach(async () => {
    await seed();
  });

  it('renders the activity-run block on the Chat page', async () => {
    const { getByTestId } = render(ChatSettings);
    expect(getByTestId('settings-activity-runs')).toBeTruthy();
    expect(getByTestId('settings-activity-run-window-rows')).toBeTruthy();
  });
});
