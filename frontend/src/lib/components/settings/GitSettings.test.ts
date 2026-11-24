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

  it.each([
    ['false when switched off', true],
    ['true when switched back on', false],
  ])('dispatches backgroundGitFetch:%s', async (_name, fetching) => {
    await seed({ backgroundGitFetch: fetching });
    const { getByLabelText } = render(GitSettings);
    const toggle = getByLabelText('Toggle Background Git Fetch') as HTMLElement;
    expect(toggle.getAttribute('aria-checked')).toBe(String(fetching));

    await fireEvent.click(toggle);
    expect(getBindingMock('UpdateSettings')!.mock.calls[0][0]).toEqual({ backgroundGitFetch: !fetching });
  });

  it('swaps the hint when the toggle is off', async () => {
    await seed({ backgroundGitFetch: false });
    const { getByText } = render(GitSettings);
    expect(getByText(/only update when you fetch, pull, or sync/i)).toBeTruthy();
    expect(getByText(/Merged PRs are only noticed for threads you have open/)).toBeTruthy();
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

  it.each([
    ['adds a valid host through the Add button', 'gitlab.mycompany.com', 'gitlab.mycompany.com'],
    ['lowercases and trims a host before saving', '  Gitlab.Example.Test  ', 'gitlab.example.test'],
  ])('%s', async (_name, typed, saved) => {
    const { getByTestId } = render(GitSettings);
    const input = getByTestId('settings-gitlab-host-input') as HTMLInputElement;
    input.value = typed;
    await fireEvent.input(input);
    await fireEvent.click(getByTestId('settings-gitlab-host-add'));

    expect(getBindingMock('UpdateSettings')!.mock.calls[0][0]).toEqual({
      gitlabSelfHostedHosts: [saved],
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

  it.each([
    ['rejects scheme-prefixed input with an inline error', [], 'https://gitlab.example.test'],
    ['rejects a duplicate host with an inline error', ['gitlab.example.test'], 'gitlab.example.test'],
    ['rejects github.com / gitlab.com as redundant', [], 'gitlab.com'],
    ['rejects single-label hostnames', [], 'localhost'],
  ])('%s', async (_name, saved, typed) => {
    await seed({ gitlabSelfHostedHosts: saved });
    const { getByTestId, queryByTestId } = render(GitSettings);
    const input = getByTestId('settings-gitlab-host-input') as HTMLInputElement;
    input.value = typed;
    await fireEvent.input(input);

    expect(queryByTestId('settings-gitlab-host-error')).toBeTruthy();
    expect((getByTestId('settings-gitlab-host-add') as HTMLButtonElement).disabled).toBe(true);
  });
});
