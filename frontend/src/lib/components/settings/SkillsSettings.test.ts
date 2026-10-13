import { beforeEach, describe, expect, it } from 'vitest';
import { render, fireEvent, waitFor } from '@testing-library/svelte';
import SkillsSettings from './SkillsSettings.svelte';
import { resetBindingMocks, setBindingMock } from '../../../test/mocks/bindings-app';
import { emitWailsEvent } from '../../../test/mocks/wailsio-runtime';

interface Home {
  provider: string;
  installed: boolean;
  complete: boolean;
  current: boolean;
  version: string;
  commit: string;
  error?: string;
}

function home(provider: string, patch: Partial<Home> = {}): Home {
  return { provider, installed: false, complete: false, current: false, version: '', commit: '', ...patch };
}

function library(homes: Home[]) {
  return { name: 'pstack', version: '0.15.15', commit: '3a604672c46cd8187d2b19980eae0a34f9f91138', skills: ['tdd', 'why'], homes };
}

const installed = { installed: true, complete: true, current: true, version: '0.15.15' };

describe('<SkillsSettings>', () => {
  beforeEach(() => {
    resetBindingMocks();
  });

  it('lists the shipped tree and each home with the action its state offers', async () => {
    setBindingMock('GetSkillsLibrary', async () =>
      library([home('claude'), home('codex', { installed: true, complete: true, current: false, version: '0.15.14' })]),
    );
    const { findByTestId, getByTestId, queryByTestId } = render(SkillsSettings);

    expect((await findByTestId('settings-skills-claude-status')).textContent?.trim()).toBe('Not installed');
    expect(getByTestId('settings-skills-claude-install')).toBeTruthy();
    expect(queryByTestId('settings-skills-claude-remove')).toBeNull();
    expect(getByTestId('settings-skills-codex-status').textContent?.trim()).toBe('0.15.14 installed, update available');
    expect(getByTestId('settings-skills-codex-update')).toBeTruthy();
    expect(getByTestId('settings-skills-codex-remove')).toBeTruthy();
    expect(queryByTestId('settings-skills-codex-install')).toBeNull();
    expect(getByTestId('settings-skills-list').textContent).toContain('tdd');
    expect(getByTestId('settings-skills').textContent).toContain('Version 0.15.15, upstream 3a60467');
  });

  it('installs into one provider and shows the home as read afterwards', async () => {
    let claude = home('claude');
    setBindingMock('GetSkillsLibrary', async () => library([claude, home('codex')]));
    const install = setBindingMock('InstallSkills', async () => {
      claude = home('claude', installed);
    });
    const { findByTestId, getByTestId, queryByTestId } = render(SkillsSettings);

    await fireEvent.click(await findByTestId('settings-skills-claude-install'));
    expect(install).toHaveBeenCalledWith('claude');
    await waitFor(() => expect(getByTestId('settings-skills-claude-status').textContent?.trim()).toBe('Installed'));
    expect(queryByTestId('settings-skills-claude-update')).toBeNull();
    expect(getByTestId('settings-skills-claude-remove')).toBeTruthy();
    expect(getByTestId('settings-skills-codex-status').textContent?.trim()).toBe('Not installed');
  });

  it('moves focus to the row\'s remaining button when the pressed one goes away', async () => {
    let claude = home('claude');
    setBindingMock('GetSkillsLibrary', async () => library([claude, home('codex')]));
    setBindingMock('InstallSkills', async () => {
      claude = home('claude', installed);
    });
    const { findByTestId, getByTestId } = render(SkillsSettings);
    const install = await findByTestId('settings-skills-claude-install');
    install.focus();
    await fireEvent.click(install);
    await waitFor(() => expect(document.activeElement).toBe(getByTestId('settings-skills-claude-remove')));
  });

  it('keeps a refused install visible beside its row and reads the home again', async () => {
    let reads = 0;
    setBindingMock('GetSkillsLibrary', async () => {
      reads++;
      return library([home('claude'), home('codex')]);
    });
    setBindingMock('InstallSkills', async () => {
      throw new Error('these skills are already in the home and were not installed by Agent Overflow: tdd. Nothing was installed.');
    });
    const { findByTestId } = render(SkillsSettings);

    await fireEvent.click(await findByTestId('settings-skills-codex-install'));
    expect((await findByTestId('settings-skills-codex-error')).textContent).toContain('Nothing was installed');
    expect(reads).toBe(2);
  });

  it('removes and updates through their own bindings', async () => {
    setBindingMock('GetSkillsLibrary', async () =>
      library([home('claude', installed), home('codex', { installed: true, complete: false, version: '0.15.15' })]),
    );
    const remove = setBindingMock('RemoveSkills', async () => {});
    const update = setBindingMock('UpdateSkills', async () => {});
    const { findByTestId, getByTestId } = render(SkillsSettings);

    expect((await findByTestId('settings-skills-codex-status')).textContent?.trim()).toBe('Install did not finish');
    await fireEvent.click(getByTestId('settings-skills-claude-remove'));
    await fireEvent.click(getByTestId('settings-skills-codex-update'));
    expect(remove).toHaveBeenCalledWith('claude');
    expect(update).toHaveBeenCalledWith('codex');
  });

  it('reads again when another client changes a home', async () => {
    let claude = home('claude');
    setBindingMock('GetSkillsLibrary', async () => library([claude, home('codex')]));
    const { findByTestId, getByTestId } = render(SkillsSettings);
    await findByTestId('settings-skills-claude-install');

    claude = home('claude', installed);
    emitWailsEvent('skills:changed', null);
    await waitFor(() => expect(getByTestId('settings-skills-claude-status').textContent?.trim()).toBe('Installed'));
  });

  it('shows a home that could not be read, with no action on it', async () => {
    setBindingMock('GetSkillsLibrary', async () =>
      library([home('claude', { error: 'read the skills manifest: permission denied' }), home('codex')]),
    );
    const { findByTestId, queryByTestId } = render(SkillsSettings);
    expect((await findByTestId('settings-skills-claude-error')).textContent).toContain('permission denied');
    expect(queryByTestId('settings-skills-claude-install')).toBeNull();
  });
});
