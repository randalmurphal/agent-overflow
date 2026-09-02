import { describe, expect, it, beforeEach } from 'vitest';
import { render, fireEvent } from '@testing-library/svelte';
import TypographySettings from './TypographySettings.svelte';
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

describe('<TypographySettings> — Font size', () => {
  beforeEach(async () => {
    await seed();
  });

  it('renders the font size input with the default value', async () => {
    const { getByTestId } = render(TypographySettings);
    const input = getByTestId('settings-font-size') as HTMLInputElement;
    expect(input.value).toBe('13');
  });

  it('dispatches fontSize patch on change', async () => {
    const { getByTestId } = render(TypographySettings);
    const input = getByTestId('settings-font-size') as HTMLInputElement;
    input.value = '16';
    await fireEvent.change(input);

    const mock = getBindingMock('UpdateSettings');
    expect(mock).toBeDefined();
    expect(mock!.mock.calls[0][0]).toEqual({ fontSize: 16 });
  });

  it('clamps below-minimum input to 10', async () => {
    const { getByTestId } = render(TypographySettings);
    const input = getByTestId('settings-font-size') as HTMLInputElement;
    input.value = '5';
    await fireEvent.change(input);

    const mock = getBindingMock('UpdateSettings');
    expect(mock!.mock.calls[0][0]).toEqual({ fontSize: 10 });
  });

  it('clamps above-maximum input to 20', async () => {
    const { getByTestId } = render(TypographySettings);
    const input = getByTestId('settings-font-size') as HTMLInputElement;
    input.value = '30';
    await fireEvent.change(input);

    const mock = getBindingMock('UpdateSettings');
    expect(mock!.mock.calls[0][0]).toEqual({ fontSize: 20 });
  });

  it('falls back to 13 on non-numeric input', async () => {
    const { getByTestId } = render(TypographySettings);
    const input = getByTestId('settings-font-size') as HTMLInputElement;
    input.value = 'banana';
    await fireEvent.change(input);

    const mock = getBindingMock('UpdateSettings');
    expect(mock!.mock.calls[0][0]).toEqual({ fontSize: 13 });
  });

  it('falls back to 13 on empty input', async () => {
    const { getByTestId } = render(TypographySettings);
    const input = getByTestId('settings-font-size') as HTMLInputElement;
    input.value = '';
    await fireEvent.change(input);

    const mock = getBindingMock('UpdateSettings');
    expect(mock!.mock.calls[0][0]).toEqual({ fontSize: 13 });
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

  it('dispatches sansFont patch on change', async () => {
    const { getByTestId } = render(TypographySettings);
    const select = getByTestId('settings-sans-font') as HTMLSelectElement;
    select.value = 'hack-nerd';
    await fireEvent.change(select);

    const mock = getBindingMock('UpdateSettings');
    expect(mock).toBeDefined();
    expect(mock!.mock.calls[0][0]).toEqual({ sansFont: 'hack-nerd' });
  });

  it('dispatches monoFont patch on change', async () => {
    const { getByTestId } = render(TypographySettings);
    const select = getByTestId('settings-mono-font') as HTMLSelectElement;
    select.value = 'system';
    await fireEvent.change(select);

    const mock = getBindingMock('UpdateSettings');
    expect(mock).toBeDefined();
    expect(mock!.mock.calls[0][0]).toEqual({ monoFont: 'system' });
  });
});
