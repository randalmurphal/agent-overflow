import { describe, expect, it, beforeEach } from 'vitest';
import { render, fireEvent } from '@testing-library/svelte';
import TypographySettings from './TypographySettings.svelte';
import { loadSettingsFixture as loadSettings } from '../../../test/helpers/settingsFixture';
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

describe('<TypographySettings> — Interface scale', () => {
  beforeEach(async () => {
    await seed();
  });

  it('shows the stored size as a percentage of the default, default selected', async () => {
    const { getByTestId } = render(TypographySettings);
    const select = getByTestId('settings-font-size') as HTMLSelectElement;
    expect(select.value).toBe('13');
    expect(select.selectedOptions[0].textContent).toBe('100% (default)');
    const labels = Array.from(select.options).map((o) => o.textContent);
    expect(labels[0]).toBe('77%');
    expect(labels[labels.length - 1]).toBe('154%');
    expect(labels).toHaveLength(11);
  });

  it.each([
    ['the pixel size behind the picked percentage', '16', 16],
    ['the default when the select carries no value', '', 13],
  ])('dispatches %s', async (_case, value, fontSize) => {
    const { getByTestId } = render(TypographySettings);
    const select = getByTestId('settings-font-size') as HTMLSelectElement;
    select.value = value;
    await fireEvent.change(select);

    const mock = getBindingMock('UpdateSettings');
    expect(mock).toBeDefined();
    expect(mock!.mock.calls[0][0]).toEqual({ fontSize });
  });

  it('reflects a chord-stepped size that is not a round percentage', async () => {
    await seed({ fontSize: 17 });
    const { getByTestId } = render(TypographySettings);
    const select = getByTestId('settings-font-size') as HTMLSelectElement;
    expect(select.value).toBe('17');
    expect(select.selectedOptions[0].textContent).toBe('131%');
  });

});
describe('<TypographySettings> — Font selectors', () => {
  beforeEach(async () => {
    await seed();
  });

  it('renders both font selectors with the default values', async () => {
    const { getByTestId } = render(TypographySettings);
    const sansSelect = getByTestId('settings-sans-font') as HTMLSelectElement;
    const monoSelect = getByTestId('settings-mono-font') as HTMLSelectElement;
    expect(sansSelect.value).toBe('geist');
    expect(monoSelect.value).toBe('geist');
    expect(sansSelect.querySelector('option[value="hack-nerd"]')).toBeTruthy();
    expect(monoSelect.querySelector('option[value="hack-nerd"]')).toBeTruthy();
    expect(sansSelect.querySelector('option[value="system"]')).toBeTruthy();
    expect(monoSelect.querySelector('option[value="system"]')).toBeTruthy();
  });

  it.each([
    ['sansFont', 'settings-sans-font', 'hack-nerd'],
    ['monoFont', 'settings-mono-font', 'system'],
  ])('dispatches %s patch on change', async (key, testId, value) => {
    const { getByTestId } = render(TypographySettings);
    const select = getByTestId(testId) as HTMLSelectElement;
    select.value = value;
    await fireEvent.change(select);

    const mock = getBindingMock('UpdateSettings');
    expect(mock).toBeDefined();
    expect(mock!.mock.calls[0][0]).toEqual({ [key]: value });
  });
});
