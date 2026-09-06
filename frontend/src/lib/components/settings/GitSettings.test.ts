import { describe, expect, it, beforeEach } from 'vitest';
import { render, fireEvent } from '@testing-library/svelte';
import GitSettings from './GitSettings.svelte';
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

describe('<GitSettings> — Repository sync', () => {
  beforeEach(async () => {
    await seed();
  });

  it('renders the background fetch toggle on by default', async () => {
    const { getByTestId, getByLabelText } = render(GitSettings);
    expect(getByTestId('settings-git-sync')).toBeTruthy();
    const toggle = getByLabelText('Toggle Background Git Fetch') as HTMLElement;
    expect(toggle.getAttribute('aria-checked')).toBe('true');
  });

  it('dispatches backgroundGitFetch:false when switched off', async () => {
    const { getByLabelText } = render(GitSettings);
    await fireEvent.click(getByLabelText('Toggle Background Git Fetch'));

    const mock = getBindingMock('UpdateSettings');
    expect(mock).toBeDefined();
    expect(mock!.mock.calls[0][0]).toEqual({ backgroundGitFetch: false });
  });

  it('dispatches backgroundGitFetch:true when switched back on', async () => {
    await seed({ backgroundGitFetch: false });
    const { getByLabelText } = render(GitSettings);
    const toggle = getByLabelText('Toggle Background Git Fetch') as HTMLElement;
    expect(toggle.getAttribute('aria-checked')).toBe('false');

    await fireEvent.click(toggle);
    const mock = getBindingMock('UpdateSettings');
    expect(mock).toBeDefined();
    expect(mock!.mock.calls[0][0]).toEqual({ backgroundGitFetch: true });
  });

  it('swaps the hint when the toggle is off', async () => {
    await seed({ backgroundGitFetch: false });
    const { getByText } = render(GitSettings);
    expect(getByText(/only update when you fetch, pull, or sync/i)).toBeTruthy();
  });
});

describe('<GitSettings> — Worktrees', () => {
  beforeEach(async () => {
    await seed();
  });

  it('dispatches worktreeBranchPrefix patch on blur', async () => {
    const { getByTestId } = render(GitSettings);
    const input = getByTestId('settings-worktree-branch-prefix') as HTMLInputElement;
    input.value = 'task-';
    await fireEvent.blur(input);

    const mock = getBindingMock('UpdateSettings');
    expect(mock).toBeDefined();
    expect(mock!.mock.calls[0][0]).toEqual({ worktreeBranchPrefix: 'task-' });
  });
});

describe('<GitSettings> — Self-hosted GitLab hosts', () => {
  beforeEach(async () => {
    await seed();
  });

  it('renders the section with the empty state by default', async () => {
    const { getByTestId } = render(GitSettings);
    expect(getByTestId('settings-gitlab-hosts')).toBeTruthy();
    expect(getByTestId('settings-gitlab-hosts-empty')).toBeTruthy();
  });

  it('adds a valid host through the Add button', async () => {
    const { getByTestId } = render(GitSettings);
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
    const { getByTestId } = render(GitSettings);
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
    const { getByTestId } = render(GitSettings);
    const remove = getByTestId('settings-gitlab-host-remove-gitlab.example.test') as HTMLButtonElement;
    await fireEvent.click(remove);

    const mock = getBindingMock('UpdateSettings');
    expect(mock!.mock.calls[0][0]).toEqual({
      gitlabSelfHostedHosts: ['gl.other.test'],
    });
  });

  it('rejects scheme-prefixed input with an inline error', async () => {
    const { getByTestId, queryByTestId } = render(GitSettings);
    const input = getByTestId('settings-gitlab-host-input') as HTMLInputElement;
    input.value = 'https://gitlab.example.test';
    await fireEvent.input(input);

    expect(queryByTestId('settings-gitlab-host-error')).toBeTruthy();
    const add = getByTestId('settings-gitlab-host-add') as HTMLButtonElement;
    expect(add.disabled).toBe(true);
  });

  it('rejects a duplicate host with an inline error', async () => {
    await seed({ gitlabSelfHostedHosts: ['gitlab.example.test'] });
    const { getByTestId, queryByTestId } = render(GitSettings);
    const input = getByTestId('settings-gitlab-host-input') as HTMLInputElement;
    input.value = 'gitlab.example.test';
    await fireEvent.input(input);

    expect(queryByTestId('settings-gitlab-host-error')).toBeTruthy();
    const add = getByTestId('settings-gitlab-host-add') as HTMLButtonElement;
    expect(add.disabled).toBe(true);
  });

  it('rejects github.com / gitlab.com as redundant', async () => {
    const { getByTestId, queryByTestId } = render(GitSettings);
    const input = getByTestId('settings-gitlab-host-input') as HTMLInputElement;
    input.value = 'gitlab.com';
    await fireEvent.input(input);

    expect(queryByTestId('settings-gitlab-host-error')).toBeTruthy();
  });

  it('rejects single-label hostnames', async () => {
    const { getByTestId, queryByTestId } = render(GitSettings);
    const input = getByTestId('settings-gitlab-host-input') as HTMLInputElement;
    input.value = 'localhost';
    await fireEvent.input(input);

    expect(queryByTestId('settings-gitlab-host-error')).toBeTruthy();
  });
});
