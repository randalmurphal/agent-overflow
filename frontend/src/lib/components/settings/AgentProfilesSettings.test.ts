import { describe, expect, it } from 'vitest';
import { fireEvent, render, within } from '@testing-library/svelte';
import AgentProfilesSettings from './AgentProfilesSettings.svelte';
import { loadSettings } from '../../stores/settings.svelte';
import { getBindingMock, setBindingMock } from '../../../test/mocks/bindings-app';
import { makeSettings } from '../../../test/helpers/settings';
import { SETTINGS_DEFAULTS } from '../../generated/settingsDefaults';
import type { AgentProfile, ProviderStatus, Settings } from '../../types/settings';

const PROFILE: AgentProfile = {
  name: 'Reviewer',
  useFor: 'Reviews.',
  provider: 'codex',
  model: 'gpt-5.6-sol',
  effort: 'high',
  runtimeMode: '',
  instructions: '',
};

function status(provider: string, installed: boolean): ProviderStatus {
  return {
    provider,
    installed,
    version: '',
    binaryPath: '',
    status: installed ? 'ready' : 'not_found',
    message: '',
  };
}

async function seed(
  overrides: Partial<Settings> = {},
  statuses: ProviderStatus[] = [status('claude', true), status('codex', true)],
): Promise<void> {
  let saved: Settings = makeSettings(overrides);
  setBindingMock('GetSettings', async () => saved);
  setBindingMock('UpdateSettings', async (patch: unknown) => {
    saved = { ...saved, ...(patch as Partial<Settings>) };
    return saved;
  });
  setBindingMock('GetProviderStatuses', async () => statuses);
  await loadSettings();
}

function lastPatch(): Record<string, unknown> {
  return getBindingMock('UpdateSettings')!.mock.calls.at(-1)![0] as Record<string, unknown>;
}

async function settle(): Promise<void> {
  for (let i = 0; i < 5; i += 1) await new Promise((resolve) => setTimeout(resolve, 0));
}

describe('<AgentProfilesSettings>', () => {
  it('shows the seeded roster and the default level', async () => {
    await seed();
    const view = render(AgentProfilesSettings);
    await settle();

    const names = view.getAllByTestId('agent-profile').map((row) => row.dataset.profileName);
    expect(names).toEqual(SETTINGS_DEFAULTS.agentProfiles.map((profile) => profile.name));
    expect(view.getAllByTestId('agent-note')).toHaveLength(SETTINGS_DEFAULTS.agentNotes.length);
    expect((view.getByTestId('agent-profiles-levels') as HTMLSelectElement).value).toBe('2');
  });

  it('saves an edited profile whole, trimmed, when the field commits', async () => {
    await seed({ agentProfiles: [PROFILE], agentNotes: [] });
    const view = render(AgentProfilesSettings);
    await settle();

    const name = view.getByLabelText('Name') as HTMLInputElement;
    await fireEvent.input(name, { target: { value: ' Critic ' } });
    expect(getBindingMock('UpdateSettings')!.mock.calls).toHaveLength(0);
    await fireEvent.change(name);

    expect(lastPatch()).toEqual({ agentProfiles: [{ ...PROFILE, name: 'Critic' }] });
  });

  // The reply to a save lands after the person has moved on to the next
  // field: it must not put back what that field held before.
  it('keeps typing that follows a save when the save returns', async () => {
    await seed({ agentProfiles: [PROFILE], agentNotes: [] });
    let finish: () => void = () => {};
    const update = getBindingMock('UpdateSettings')!.getMockImplementation()!;
    setBindingMock('UpdateSettings', (patch: unknown) =>
      new Promise((resolve) => {
        finish = () => resolve(update(patch));
      }),
    );
    const view = render(AgentProfilesSettings);
    await settle();

    const name = view.getByLabelText('Name') as HTMLInputElement;
    await fireEvent.input(name, { target: { value: 'Critic' } });
    await fireEvent.change(name);
    const useFor = view.getByLabelText('Use for') as HTMLTextAreaElement;
    await fireEvent.input(useFor, { target: { value: 'Reviews, sharply.' } });
    finish();
    await settle();

    expect(useFor.value).toBe('Reviews, sharply.');
    expect(name.value).toBe('Critic');
  });

  // A change made elsewhere (another window, a paired computer's page)
  // replaces the draft when it holds nothing unsaved.
  it('follows a roster saved elsewhere', async () => {
    await seed({ agentProfiles: [PROFILE], agentNotes: [] });
    const view = render(AgentProfilesSettings);
    await settle();

    const { applySettingsSnapshot } = await import('../../stores/settings.svelte');
    applySettingsSnapshot(makeSettings({ agentProfiles: [{ ...PROFILE, name: 'Renamed' }], agentNotes: [] }));
    await settle();

    expect((view.getByLabelText('Name') as HTMLInputElement).value).toBe('Renamed');
  });

  it('holds back an edit the backend would refuse and says why', async () => {
    await seed({ agentProfiles: [PROFILE, { ...PROFILE, name: 'Other' }], agentNotes: [] });
    const view = render(AgentProfilesSettings);
    await settle();

    const second = view.getAllByTestId('agent-profile')[1];
    const name = within(second).getByLabelText('Name') as HTMLInputElement;
    await fireEvent.input(name, { target: { value: 'reviewer' } });
    await fireEvent.change(name);

    expect(getBindingMock('UpdateSettings')!.mock.calls).toHaveLength(0);
    expect(within(second).getByTestId('agent-profile-error').textContent).toBe(
      'Another profile is already named reviewer.',
    );
    // The draft keeps what was typed.
    expect(name.value).toBe('reviewer');

    const model = within(second).getByLabelText('Model') as HTMLInputElement;
    const provider = within(second).getByLabelText('Provider') as HTMLSelectElement;
    await fireEvent.input(name, { target: { value: 'Other' } });
    await fireEvent.change(provider, { target: { value: '' } });
    expect(within(second).getByTestId('agent-profile-error').textContent).toBe(
      'A model needs the provider that runs it.',
    );
    expect(model.value).toBe('gpt-5.6-sol');
    expect(getBindingMock('UpdateSettings')!.mock.calls).toHaveLength(0);
  });

  it('adds and deletes profiles and notes', async () => {
    await seed({ agentProfiles: [PROFILE], agentNotes: [{ text: 'Keep it short.', when: '' }] });
    const view = render(AgentProfilesSettings);
    await settle();

    await fireEvent.click(view.getByRole('button', { name: 'Add profile' }));
    const added = view.getAllByTestId('agent-profile')[1];
    await fireEvent.input(within(added).getByLabelText('Name'), { target: { value: 'Mine' } });
    const useFor = within(added).getByLabelText('Use for');
    await fireEvent.input(useFor, { target: { value: 'Anything.' } });
    await fireEvent.change(useFor);
    expect(lastPatch()).toEqual({
      agentProfiles: [
        PROFILE,
        { name: 'Mine', useFor: 'Anything.', provider: '', model: '', effort: '', runtimeMode: '', instructions: '' },
      ],
    });

    await fireEvent.click(view.getByRole('button', { name: 'Delete Reviewer' }));
    expect((lastPatch().agentProfiles as AgentProfile[]).map((profile) => profile.name)).toEqual(['Mine']);

    const when = view.getByLabelText('Shown') as HTMLSelectElement;
    await fireEvent.change(when, { target: { value: 'both_providers' } });
    expect(lastPatch()).toEqual({ agentNotes: [{ text: 'Keep it short.', when: 'both_providers' }] });

    const note = view.getByTestId('agent-note');
    await fireEvent.click(within(note).getByRole('button', { name: 'Delete' }));
    expect(lastPatch()).toEqual({ agentNotes: [] });
    expect(view.queryAllByTestId('agent-note')).toHaveLength(0);
  });

  it('restores the shipped roster in one write', async () => {
    await seed({ agentProfiles: [PROFILE], agentNotes: [] });
    const view = render(AgentProfilesSettings);
    await settle();

    await fireEvent.click(view.getByTestId('agent-profiles-restore'));
    expect(lastPatch()).toEqual({
      agentProfiles: SETTINGS_DEFAULTS.agentProfiles,
      agentNotes: SETTINGS_DEFAULTS.agentNotes,
    });
    expect(view.getAllByTestId('agent-profile')).toHaveLength(SETTINGS_DEFAULTS.agentProfiles.length);
  });

  it('sends the level as a number', async () => {
    await seed();
    const view = render(AgentProfilesSettings);
    await settle();

    await fireEvent.change(view.getByTestId('agent-profiles-levels'), { target: { value: '4' } });
    expect(lastPatch()).toEqual({ subThreadLevels: 4 });
  });

  // A profile on a provider this computer does not run stays editable and
  // says it is not offered here; one on a provider that runs says nothing.
  it.each([
    ['Codex is not installed', {}, [status('claude', true), status('codex', false)]],
    ['Codex is turned off', { codexEnabled: false }, [status('claude', true), status('codex', true)]],
  ] as const)('marks a Codex profile when %s', async (_, overrides, statuses) => {
    await seed({ ...overrides, agentProfiles: [PROFILE, { ...PROFILE, name: 'Claude one', provider: 'claude', model: '' }] }, [...statuses]);
    const view = render(AgentProfilesSettings);
    await settle();

    const [codex, claude] = view.getAllByTestId('agent-profile');
    expect(within(codex).getByTestId('agent-profile-unavailable').textContent).toContain(
      'Codex does not run on this computer, so threads here are not offered this profile.',
    );
    expect(within(claude).queryByTestId('agent-profile-unavailable')).toBeNull();
    expect((within(codex).getByLabelText('Name') as HTMLInputElement).disabled).toBe(false);
    expect(within(codex).queryByTestId('agent-profile-error')).toBeNull();
  });

  it('marks nothing when both providers run', async () => {
    await seed({ agentProfiles: [PROFILE] });
    const view = render(AgentProfilesSettings);
    await settle();
    expect(view.queryByTestId('agent-profile-unavailable')).toBeNull();
  });

  it('keeps an emptied roster empty rather than showing the seeds', async () => {
    await seed({ agentProfiles: [], agentNotes: [] });
    const view = render(AgentProfilesSettings);
    await settle();
    expect(view.queryAllByTestId('agent-profile')).toHaveLength(0);
    expect(view.getByText('No profiles. Threads spawn with their own provider and model.')).toBeTruthy();
  });
});
