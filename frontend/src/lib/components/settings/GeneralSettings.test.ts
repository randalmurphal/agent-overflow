import { describe, expect, it, beforeEach } from 'vitest';
import { render, fireEvent } from '@testing-library/svelte';
import GeneralSettings from './GeneralSettings.svelte';
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

describe('<GeneralSettings> — Thread defaults section', () => {
  beforeEach(async () => {
    await seed();
  });

  it('renders the workspace seed settings without chat default controls', async () => {
    const { getByTestId } = render(GeneralSettings);
    expect(getByTestId('settings-thread-defaults')).toBeTruthy();
    expect(getByTestId('settings-default-thread-env-mode')).toBeTruthy();
    expect(getByTestId('settings-worktree-branch-prefix')).toBeTruthy();
  });

  it('dispatches defaultThreadEnvMode patch on change', async () => {
    const { getByTestId } = render(GeneralSettings);
    const select = getByTestId('settings-default-thread-env-mode') as HTMLSelectElement;
    select.value = 'worktree';
    await fireEvent.change(select);

    const mock = getBindingMock('UpdateSettings');
    expect(mock).toBeDefined();
    expect(mock!.mock.calls[0][0]).toEqual({ defaultThreadEnvMode: 'worktree' });
  });

  it('dispatches worktreeBranchPrefix patch on blur', async () => {
    const { getByTestId } = render(GeneralSettings);
    const input = getByTestId('settings-worktree-branch-prefix') as HTMLInputElement;
    input.value = 'task-';
    await fireEvent.blur(input);

    const mock = getBindingMock('UpdateSettings');
    expect(mock).toBeDefined();
    expect(mock!.mock.calls[0][0]).toEqual({ worktreeBranchPrefix: 'task-' });
  });
});

describe('<GeneralSettings> — Pane density', () => {
  beforeEach(async () => {
    await seed();
  });

  it('dispatches paneDensity patch on change', async () => {
    const { getByTestId } = render(GeneralSettings);
    const option = getByTestId('pane-density-option-spacious');
    const input = option.querySelector('input[type="radio"]') as HTMLInputElement;
    await fireEvent.click(input);

    const mock = getBindingMock('UpdateSettings');
    expect(mock).toBeDefined();
    expect(mock!.mock.calls[0][0]).toEqual({ paneDensity: 'spacious' });
  });
});

describe('<GeneralSettings> — Retention', () => {
  beforeEach(async () => {
    await seed();
  });

  it('renders the retention input with the default value', async () => {
    const { getByTestId } = render(GeneralSettings);
    expect(getByTestId('settings-retention')).toBeTruthy();
    const input = getByTestId('settings-retention-days') as HTMLInputElement;
    expect(input.value).toBe('30');
  });

  it('shows the disabled-cleanup hint when retention days is 0', async () => {
    await seed({ retention: { days: 0 } });
    const { getByText } = render(GeneralSettings);
    expect(
      getByText(/automatic cleanup is disabled/i),
    ).toBeTruthy();
  });

  it('dispatches retention patch on blur with parsed integer', async () => {
    const { getByTestId } = render(GeneralSettings);
    const input = getByTestId('settings-retention-days') as HTMLInputElement;
    input.value = '7';
    await fireEvent.blur(input);

    const mock = getBindingMock('UpdateSettings');
    expect(mock).toBeDefined();
    expect(mock!.mock.calls[0][0]).toEqual({ retention: { days: 7 } });
  });

  it('coerces non-numeric input to 0 (disabled)', async () => {
    const { getByTestId } = render(GeneralSettings);
    const input = getByTestId('settings-retention-days') as HTMLInputElement;
    input.value = 'banana';
    await fireEvent.blur(input);

    const mock = getBindingMock('UpdateSettings');
    expect(mock).toBeDefined();
    expect(mock!.mock.calls[0][0]).toEqual({ retention: { days: 0 } });
  });

  it('coerces negative input to 0 (disabled)', async () => {
    const { getByTestId } = render(GeneralSettings);
    const input = getByTestId('settings-retention-days') as HTMLInputElement;
    input.value = '-5';
    await fireEvent.blur(input);

    const mock = getBindingMock('UpdateSettings');
    expect(mock).toBeDefined();
    expect(mock!.mock.calls[0][0]).toEqual({ retention: { days: 0 } });
  });
});

describe('<GeneralSettings> — Self-hosted GitLab hosts', () => {
  beforeEach(async () => {
    await seed();
  });

  it('renders the section with the empty state by default', async () => {
    const { getByTestId } = render(GeneralSettings);
    expect(getByTestId('settings-gitlab-hosts')).toBeTruthy();
    expect(getByTestId('settings-gitlab-hosts-empty')).toBeTruthy();
  });

  it('adds a valid host through the Add button', async () => {
    const { getByTestId } = render(GeneralSettings);
    const input = getByTestId('settings-gitlab-host-input') as HTMLInputElement;
    const add = getByTestId('settings-gitlab-host-add') as HTMLButtonElement;
    input.value = 'gitlab.mycompany.com';
    await fireEvent.input(input);
    await fireEvent.click(add);

    const mock = getBindingMock('UpdateSettings');
    expect(mock).toBeDefined();
    expect(mock!.mock.calls[0][0]).toEqual({
      gitlabSelfHostedHosts: ['gitlab.mycompany.com'],
    });
  });

  it('lowercases and trims a host before saving', async () => {
    const { getByTestId } = render(GeneralSettings);
    const input = getByTestId('settings-gitlab-host-input') as HTMLInputElement;
    const add = getByTestId('settings-gitlab-host-add') as HTMLButtonElement;
    input.value = '  Gitlab.Example.Test  ';
    await fireEvent.input(input);
    await fireEvent.click(add);

    const mock = getBindingMock('UpdateSettings');
    expect(mock!.mock.calls[0][0]).toEqual({
      gitlabSelfHostedHosts: ['gitlab.example.test'],
    });
  });

  it('removes a host when its Remove button is clicked', async () => {
    await seed({ gitlabSelfHostedHosts: ['gitlab.example.test', 'gl.other.test'] });
    const { getByTestId } = render(GeneralSettings);
    const remove = getByTestId('settings-gitlab-host-remove-gitlab.example.test') as HTMLButtonElement;
    await fireEvent.click(remove);

    const mock = getBindingMock('UpdateSettings');
    expect(mock!.mock.calls[0][0]).toEqual({
      gitlabSelfHostedHosts: ['gl.other.test'],
    });
  });

  it('rejects scheme-prefixed input with an inline error', async () => {
    const { getByTestId, queryByTestId } = render(GeneralSettings);
    const input = getByTestId('settings-gitlab-host-input') as HTMLInputElement;
    input.value = 'https://gitlab.example.test';
    await fireEvent.input(input);

    expect(queryByTestId('settings-gitlab-host-error')).toBeTruthy();
    const add = getByTestId('settings-gitlab-host-add') as HTMLButtonElement;
    expect(add.disabled).toBe(true);
  });

  it('rejects a duplicate host with an inline error', async () => {
    await seed({ gitlabSelfHostedHosts: ['gitlab.example.test'] });
    const { getByTestId, queryByTestId } = render(GeneralSettings);
    const input = getByTestId('settings-gitlab-host-input') as HTMLInputElement;
    input.value = 'gitlab.example.test';
    await fireEvent.input(input);

    expect(queryByTestId('settings-gitlab-host-error')).toBeTruthy();
    const add = getByTestId('settings-gitlab-host-add') as HTMLButtonElement;
    expect(add.disabled).toBe(true);
  });

  it('rejects github.com / gitlab.com as redundant', async () => {
    const { getByTestId, queryByTestId } = render(GeneralSettings);
    const input = getByTestId('settings-gitlab-host-input') as HTMLInputElement;
    input.value = 'gitlab.com';
    await fireEvent.input(input);

    expect(queryByTestId('settings-gitlab-host-error')).toBeTruthy();
  });

  it('rejects single-label hostnames', async () => {
    const { getByTestId, queryByTestId } = render(GeneralSettings);
    const input = getByTestId('settings-gitlab-host-input') as HTMLInputElement;
    input.value = 'localhost';
    await fireEvent.input(input);

    expect(queryByTestId('settings-gitlab-host-error')).toBeTruthy();
  });
});

describe('<GeneralSettings> — Font selectors', () => {
  beforeEach(async () => {
    await seed();
  });

  it('renders both font selectors with the default values', async () => {
    const { getByTestId } = render(GeneralSettings);
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
    const { getByTestId } = render(GeneralSettings);
    const select = getByTestId('settings-sans-font') as HTMLSelectElement;
    select.value = 'hack-nerd';
    await fireEvent.change(select);

    const mock = getBindingMock('UpdateSettings');
    expect(mock).toBeDefined();
    expect(mock!.mock.calls[0][0]).toEqual({ sansFont: 'hack-nerd' });
  });

  it('dispatches monoFont patch on change', async () => {
    const { getByTestId } = render(GeneralSettings);
    const select = getByTestId('settings-mono-font') as HTMLSelectElement;
    select.value = 'system';
    await fireEvent.change(select);

    const mock = getBindingMock('UpdateSettings');
    expect(mock).toBeDefined();
    expect(mock!.mock.calls[0][0]).toEqual({ monoFont: 'system' });
  });
});
