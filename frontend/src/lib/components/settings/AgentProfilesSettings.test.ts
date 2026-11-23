import { describe, expect, it, vi } from 'vitest';
import { fireEvent, render, within } from '@testing-library/svelte';
import AgentProfilesSettings from './AgentProfilesSettings.svelte';
import pageSource from './AgentProfilesSettings.svelte?raw';
import { applySettingsSnapshot, loadSettings, resetSettingsForTest } from '../../stores/settings.svelte';
import ComputerSettingsPage from './ComputerSettingsPage.svelte';
import { resetStagedBackends, stageBackend } from '../../../test/helpers/backends';
import { methodNotFound, settingsAsV0019 } from '../../../test/helpers/olderComputer';
import { getBindingMock, setBindingMock } from '../../../test/mocks/bindings-app';
import { makeSettings } from '../../../test/helpers/settings';
import { TransportError } from '../../transport/wsClient';
import { takePinnedBackend } from '../../transport/backends';
import type { AgentNote, AgentProfile, Settings } from '../../types/settings';
import { RESTORE_OVERTAKEN } from './rosterEditor.svelte';

const PROFILE: AgentProfile = {
  name: 'Reviewer',
  useFor: 'Reviews.',
  provider: 'codex',
  model: 'gpt-5.6-sol',
  effort: 'high',
  runtimeMode: '',
  instructions: '',
};

type Check = { problem?: string; missing?: string; runs?: string; unreadable?: string[]; family?: boolean };

/** The roster the computer's own release starts with (AgentRosterSeeds). */
const SEEDS: { profiles: AgentProfile[]; notes: AgentNote[] } = {
  profiles: [
    { ...PROFILE, name: 'Seeded', useFor: 'What that computer ships.', provider: 'claude', model: 'claude-opus' },
    { ...PROFILE, name: 'Seeded Codex', useFor: 'Its Codex seed.', model: 'gpt-sol' },
  ],
  notes: [{ text: 'A note that computer ships.', when: '' }],
};

/**
 * Loads `overrides` as the saved settings, with UpdateSettings keeping what
 * it is sent and CheckAgentProfiles answering `check` for each profile.
 */
async function seed(
  overrides: Partial<Settings> = {},
  check: (profile: AgentProfile) => Check = () => ({}),
): Promise<void> {
  let saved: Settings = makeSettings(overrides);
  setBindingMock('GetSettings', async () => saved);
  setBindingMock('UpdateSettings', async (patch: unknown) => {
    saved = { ...saved, ...(patch as Partial<Settings>) };
    return saved;
  });
  setBindingMock('CheckAgentProfiles', vi.fn(async (profiles: AgentProfile[]) => profiles.map(check)));
  setBindingMock('AgentRosterSeeds', async () => SEEDS);
  await loadSettings();
}

/** A change saved elsewhere: another window, or a paired computer's page. */
function savedElsewhere(lists: { agentProfiles?: AgentProfile[]; agentNotes?: AgentNote[] }): void {
  applySettingsSnapshot(makeSettings({ agentProfiles: [PROFILE], agentNotes: [NOTE], ...lists }));
}

const NOTE: AgentNote = { text: 'Keep it short.', when: '' };

function lastPatch(): Record<string, unknown> {
  return getBindingMock('UpdateSettings')!.mock.calls.at(-1)![0] as Record<string, unknown>;
}

async function settle(): Promise<void> {
  for (let i = 0; i < 5; i += 1) await new Promise((resolve) => setTimeout(resolve, 0));
}

describe('<AgentProfilesSettings>', () => {
  it('shows the computer\'s roster and the default level', async () => {
    await seed({ agentProfiles: SEEDS.profiles, agentNotes: SEEDS.notes });
    const view = render(AgentProfilesSettings);
    await settle();

    const names = view.getAllByTestId('agent-profile').map((row) => row.dataset.profileName);
    expect(names).toEqual(SEEDS.profiles.map((profile) => profile.name));
    expect(view.getAllByTestId('agent-note')).toHaveLength(SEEDS.notes.length);
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
    await settle();

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
    await settle();
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
    await settle();
    expect(lastPatch()).toEqual({
      agentProfiles: [
        PROFILE,
        { name: 'Mine', useFor: 'Anything.', provider: '', model: '', effort: '', runtimeMode: '', instructions: '' },
      ],
    });

    await fireEvent.click(view.getByRole('button', { name: 'Delete Reviewer' }));
    await settle();
    expect((lastPatch().agentProfiles as AgentProfile[]).map((profile) => profile.name)).toEqual(['Mine']);

    const when = view.getByLabelText('Shown') as HTMLSelectElement;
    await fireEvent.change(when, { target: { value: 'both_providers' } });
    expect(lastPatch()).toEqual({ agentNotes: [{ text: 'Keep it short.', when: 'both_providers' }] });

    const note = view.getByTestId('agent-note');
    await fireEvent.click(within(note).getByRole('button', { name: 'Delete' }));
    expect(lastPatch()).toEqual({ agentNotes: [] });
    expect(view.queryAllByTestId('agent-note')).toHaveLength(0);
  });

  // A save checks the profiles before it writes; Restore replaces the
  // draft that check is for, even when the saved roster is already the
  // shipped one and nothing else marks the change.
  it('keeps the restored roster when a save that was checking returns', async () => {
    await seed({ agentProfiles: SEEDS.profiles, agentNotes: SEEDS.notes });
    const view = render(AgentProfilesSettings);
    await settle();
    const releases: (() => void)[] = [];
    setBindingMock('CheckAgentProfiles', (profiles: AgentProfile[]) =>
      new Promise((resolve) => {
        releases.push(() => resolve(profiles.map(() => ({}))));
      }),
    );

    const name = view.getAllByLabelText('Name')[0] as HTMLInputElement;
    await fireEvent.input(name, { target: { value: 'Discarded' } });
    await fireEvent.change(name);
    await fireEvent.click(view.getByTestId('agent-profiles-restore'));
    await settle();
    // The save's check, then the check of the restored profiles.
    expect(releases).toHaveLength(2);
    for (const release of releases) release();
    await settle();

    expect(lastPatch()).toEqual({ agentProfiles: SEEDS.profiles, agentNotes: SEEDS.notes });
    expect(getBindingMock('UpdateSettings')!.mock.calls).toHaveLength(1);
    expect((view.getAllByLabelText('Name')[0] as HTMLInputElement).value).toBe(SEEDS.profiles[0].name);
  });

  // The roster Restore writes is the one the computer itself starts with,
  // asked of that computer, not this window's.
  it('restores the computer\'s own starting roster in one write', async () => {
    await seed({ agentProfiles: [PROFILE], agentNotes: [] });
    stageBackend();
    try {
      applySettingsSnapshot(makeSettings({ agentProfiles: [PROFILE], agentNotes: [] }), 'laptop');
      const asked: (string | null)[] = [];
      setBindingMock('AgentRosterSeeds', async () => {
        asked.push(takePinnedBackend());
        return SEEDS;
      });
      const view = render(ComputerSettingsPage, { props: { backend: 'laptop', Page: AgentProfilesSettings, needsComputer: false } });
      await settle();

      await fireEvent.click(view.getByTestId('agent-profiles-restore'));
      await settle();
      expect(asked).toEqual(['laptop']);
      expect(lastPatch()).toEqual({ agentProfiles: SEEDS.profiles, agentNotes: SEEDS.notes });
      expect(view.getAllByTestId('agent-profile')).toHaveLength(SEEDS.profiles.length);
      expect(view.queryByTestId('agent-profiles-restore-refusal')).toBeNull();
    } finally {
      resetStagedBackends();
      resetSettingsForTest();
    }
  });

  // Restore belongs to the gesture that started it: while the computer
  // answers with its starting roster, anything newer the person does, or
  // the page closing, retires it, and its late answer writes nothing.
  describe('a Restore waiting on the starting roster', () => {
    async function restoreWaiting() {
      await seed({ agentProfiles: [PROFILE], agentNotes: [NOTE] });
      const answers: (() => void)[] = [];
      setBindingMock('AgentRosterSeeds', () => new Promise((resolve) => answers.push(() => resolve(SEEDS))));
      const view = render(AgentProfilesSettings);
      await settle();
      await fireEvent.click(view.getByTestId('agent-profiles-restore'));
      await settle();
      return { view, answers };
    }

    it('keeps an edit made meanwhile and writes nothing', async () => {
      const { view, answers } = await restoreWaiting();
      const name = view.getByLabelText('Name') as HTMLInputElement;
      await fireEvent.input(name, { target: { value: 'Typed meanwhile' } });
      answers[0]();
      await settle();

      expect(name.value).toBe('Typed meanwhile');
      expect(getBindingMock('UpdateSettings')!.mock.calls).toHaveLength(0);
      // Retired by the person's own newer action, it says nothing.
      expect(view.queryByTestId('agent-profiles-restore-refusal')).toBeNull();
    });

    it('keeps a save made meanwhile and writes nothing over it', async () => {
      const { view, answers } = await restoreWaiting();
      const name = view.getByLabelText('Name') as HTMLInputElement;
      await fireEvent.input(name, { target: { value: 'Saved meanwhile' } });
      await fireEvent.change(name);
      const note = view.getByLabelText('Note') as HTMLTextAreaElement;
      await fireEvent.input(note, { target: { value: 'A note saved meanwhile.' } });
      await fireEvent.change(note);
      await settle();
      answers[0]();
      await settle();

      const writes = getBindingMock('UpdateSettings')!.mock.calls.map((call) => call[0]);
      expect(writes).toHaveLength(2);
      expect(writes).toEqual(expect.arrayContaining([
        { agentNotes: [{ text: 'A note saved meanwhile.', when: '' }] },
        { agentProfiles: [{ ...PROFILE, name: 'Saved meanwhile' }] },
      ]));
      expect((view.getByLabelText('Name') as HTMLInputElement).value).toBe('Saved meanwhile');
      expect((view.getByLabelText('Note') as HTMLTextAreaElement).value).toBe('A note saved meanwhile.');
    });

    it('keeps a menu choice whose save is still checking, and writes nothing over it', async () => {
      const { view, answers } = await restoreWaiting();
      let checked: () => void = () => {};
      setBindingMock('CheckAgentProfiles', (profiles: AgentProfile[]) =>
        new Promise((resolve) => {
          checked = () => resolve(profiles.map(() => ({})));
        }),
      );
      await fireEvent.change(view.getByLabelText('Effort'), { target: { value: 'low' } });
      await settle();
      answers[0]();
      await settle();
      checked();
      await settle();

      expect(getBindingMock('UpdateSettings')!.mock.calls.map((call) => call[0])).toEqual([
        { agentProfiles: [{ ...PROFILE, effort: 'low' }] },
      ]);
    });

    it('keeps a profile added meanwhile', async () => {
      const { view, answers } = await restoreWaiting();
      await fireEvent.click(view.getByRole('button', { name: 'Add profile' }));
      answers[0]();
      await settle();

      expect(view.getAllByTestId('agent-profile')).toHaveLength(2);
      expect(getBindingMock('UpdateSettings')!.mock.calls).toHaveLength(0);
    });

    it('keeps a roster saved elsewhere meanwhile', async () => {
      const { view, answers } = await restoreWaiting();
      savedElsewhere({ agentProfiles: [{ ...PROFILE, name: 'From elsewhere' }] });
      await settle();
      answers[0]();
      await settle();

      expect((view.getByLabelText('Name') as HTMLInputElement).value).toBe('From elsewhere');
      expect(getBindingMock('UpdateSettings')!.mock.calls).toHaveLength(0);
      expect(view.getByTestId('agent-profiles-restore-refusal').textContent).toBe(RESTORE_OVERTAKEN);
    });

    it('writes once, for the later of two Restores', async () => {
      const { view, answers } = await restoreWaiting();
      await fireEvent.click(view.getByTestId('agent-profiles-restore'));
      await settle();
      answers[0]();
      await settle();
      expect(getBindingMock('UpdateSettings')!.mock.calls).toHaveLength(0);
      answers[1]();
      await settle();

      expect(getBindingMock('UpdateSettings')!.mock.calls.map((call) => call[0])).toEqual([
        { agentProfiles: SEEDS.profiles, agentNotes: SEEDS.notes },
      ]);
    });

    it('writes nothing once the page is closed', async () => {
      const { view, answers } = await restoreWaiting();
      view.unmount();
      answers[0]();
      await settle();

      expect(getBindingMock('UpdateSettings')!.mock.calls).toHaveLength(0);
    });
  });

  // A computer with a roster but without AgentRosterSeeds predates model
  // families: Restore there writes nothing and says why, and a family typed
  // there that it does not offer is held back with the reason. An exact
  // model it lacks is still only a note, and the same family saves on a
  // computer that reads families.
  describe('on a computer that predates model families', () => {
    const LACKS = 'Claude Code does not offer model "claude-opus" on this computer. It offers: claude-opus-4-8.';
    async function olderThere() {
      await seed({ agentProfiles: [PROFILE], agentNotes: [] });
      stageBackend();
      applySettingsSnapshot(makeSettings({ agentProfiles: [PROFILE], agentNotes: [] }), 'laptop');
      setBindingMock('AgentRosterSeeds', async () => {
        if (takePinnedBackend() === 'laptop') throw methodNotFound();
        return SEEDS;
      });
      // There, a model it does not list is missing; here, the version
      // rules say which models are families.
      setBindingMock('CheckAgentProfiles', async (profiles: AgentProfile[]) => {
        const there = takePinnedBackend() === 'laptop';
        return profiles.map((profile) =>
          there
            ? profile.model.startsWith('claude-opus') && profile.model !== 'claude-opus-4-8' ? { missing: LACKS } : {}
            : { family: profile.model === 'claude-opus' },
        );
      });
      const view = render(ComputerSettingsPage, { props: { backend: 'laptop', Page: AgentProfilesSettings, needsComputer: false } });
      await settle();
      return view;
    }

    it('refuses Restore and says why', async () => {
      try {
        const view = await olderThere();
        await fireEvent.click(view.getByTestId('agent-profiles-restore'));
        await settle();
        expect(view.getByTestId('agent-profiles-restore-refusal').textContent).toBe(
          'Laptop runs an older version: it has no starting roster of its own to restore.',
        );
        expect(getBindingMock('UpdateSettings')!.mock.calls).toHaveLength(0);
        expect(view.getAllByTestId('agent-profile').map((row) => row.dataset.profileName)).toEqual([PROFILE.name]);
      } finally {
        resetStagedBackends();
        resetSettingsForTest();
      }
    });

    it('holds back a family it does not offer and says why, and saves an exact model', async () => {
      try {
        const view = await olderThere();
        await fireEvent.change(view.getByLabelText('Provider'), { target: { value: 'claude' } });
        const model = view.getByLabelText('Model') as HTMLInputElement;
        await fireEvent.input(model, { target: { value: 'claude-opus' } });
        await fireEvent.change(model);
        await settle();
        expect(view.getByTestId('agent-profile-error').textContent).toBe(
          `Laptop runs an older version: it has no model families. ${LACKS}`,
        );
        const writes = getBindingMock('UpdateSettings')!.mock.calls.filter((call) => {
          const profiles = (call[0] as Partial<Settings>).agentProfiles;
          return profiles?.some((profile) => profile.model === 'claude-opus');
        });
        expect(writes).toHaveLength(0);

        await fireEvent.input(model, { target: { value: 'claude-opus-5' } });
        await fireEvent.change(model);
        await settle();
        expect(view.queryByTestId('agent-profile-error')).toBeNull();
        expect(view.getByTestId('agent-profile-unavailable').textContent).toContain(LACKS);
        expect(lastPatch()).toEqual({ agentProfiles: [{ ...PROFILE, provider: 'claude', model: 'claude-opus-5' }] });
      } finally {
        resetStagedBackends();
        resetSettingsForTest();
      }
    });
  });

  it('saves a family a computer that reads families does not list, with the note', async () => {
    await seed({ agentProfiles: [PROFILE], agentNotes: [] }, (profile) =>
      profile.model === 'claude-opus' ? { missing: 'Claude Code offers no claude-opus model on this computer.', family: true } : {},
    );
    const view = render(AgentProfilesSettings);
    await settle();

    await fireEvent.change(view.getByLabelText('Provider'), { target: { value: 'claude' } });
    const model = view.getByLabelText('Model') as HTMLInputElement;
    await fireEvent.input(model, { target: { value: 'claude-opus' } });
    await fireEvent.change(model);
    await settle();
    expect(view.queryByTestId('agent-profile-error')).toBeNull();
    expect(lastPatch()).toEqual({ agentProfiles: [{ ...PROFILE, provider: 'claude', model: 'claude-opus' }] });
  });

  it('sends the level as a number', async () => {
    await seed();
    const view = render(AgentProfilesSettings);
    await settle();

    await fireEvent.change(view.getByTestId('agent-profiles-levels'), { target: { value: '4' } });
    expect(lastPatch()).toEqual({ subThreadLevels: 4 });
  });

  // What the computer makes of a profile (CheckAgentProfiles): one it
  // lacks the provider or model for stays editable and says it is not
  // offered here; one whose effort its model lacks is held back.
  it('marks a profile this computer cannot offer, and says nothing of one it can', async () => {
    await seed(
      { agentProfiles: [PROFILE, { ...PROFILE, name: 'Claude one', provider: 'claude', model: '' }] },
      (profile) => (profile.provider === 'codex' ? { missing: 'Codex does not run on this computer.' } : {}),
    );
    const view = render(AgentProfilesSettings);
    await settle();

    const [codex, claude] = view.getAllByTestId('agent-profile');
    expect(within(codex).getByTestId('agent-profile-unavailable').textContent).toBe(
      'Codex does not run on this computer. Threads here are not offered this profile; a paired computer that runs it still offers it.',
    );
    expect(within(claude).queryByTestId('agent-profile-unavailable')).toBeNull();
    expect((within(codex).getByLabelText('Name') as HTMLInputElement).disabled).toBe(false);
    expect(within(codex).queryByTestId('agent-profile-error')).toBeNull();
  });

  // A profile on a family says which model it runs here, and which listed
  // models the version rule cannot read; one on an exact model says
  // neither, since it runs what it names.
  it('says which model a family profile runs here', async () => {
    await seed(
      {
        agentProfiles: [
          { ...PROFILE, name: 'Opus', provider: 'claude', model: 'claude-opus' },
          { ...PROFILE, name: 'Pinned', provider: 'claude', model: 'claude-opus-5' },
        ],
      },
      (profile) =>
        profile.model === 'claude-opus'
          ? { runs: 'claude-opus-5-5', unreadable: ['claude-opus-6-preview'] }
          : { runs: profile.model },
    );
    const view = render(AgentProfilesSettings);
    await settle();

    const [family, pinned] = view.getAllByTestId('agent-profile');
    expect(within(family).getByTestId('agent-profile-runs').textContent).toBe(
      'Runs claude-opus-5-5 here, the newest claude-opus model this computer lists.',
    );
    expect(within(family).getByTestId('agent-profile-unreadable').textContent).toBe(
      'Also listed here, but never run for a family: claude-opus-6-preview. Agent Overflow cannot read their version.',
    );
    expect(within(pinned).queryByTestId('agent-profile-runs')).toBeNull();
    expect(within(pinned).queryByTestId('agent-profile-unreadable')).toBeNull();
  });

  it('holds back an effort the model does not offer and says why', async () => {
    const problem = 'claude-haiku-4 takes no effort setting; leave effort unset.';
    await seed({ agentProfiles: [PROFILE] }, (profile) => (profile.effort === 'max' ? { problem } : {}));
    const view = render(AgentProfilesSettings);
    await settle();

    await fireEvent.change(view.getByLabelText('Effort'), { target: { value: 'max' } });
    await settle();
    expect(getBindingMock('UpdateSettings')!.mock.calls).toHaveLength(0);
    expect(view.getByTestId('agent-profile-error').textContent).toBe(problem);
    expect((view.getByLabelText('Effort') as HTMLSelectElement).value).toBe('max');

    await fireEvent.change(view.getByLabelText('Effort'), { target: { value: 'high' } });
    await settle();
    expect(view.queryByTestId('agent-profile-error')).toBeNull();
    expect(lastPatch()).toEqual({ agentProfiles: [PROFILE] });
  });

  // Whether a family runs on a computer takes up to three lookups: that
  // computer's check, whether it reads families (AgentRosterSeeds), and
  // which models are families (this window's own check). When one the
  // answer needs fails, or has no answer where one is needed, the profiles
  // stay unsaved and the page says why; a later check that answers clears
  // it.
  describe('when a lookup the check needs fails', () => {
    type Lookup = 'answers' | 'rejects' | 'older';
    const LACKS = 'Claude Code does not offer model "claude-opus" on this computer.';
    const familyWrites = () =>
      getBindingMock('UpdateSettings')!.mock.calls.filter((call) =>
        (call[0] as Partial<Settings>).agentProfiles?.some((profile) => profile.model === 'claude-opus'),
      );

    async function typeFamily(lookups: { check?: Lookup; seeds?: Lookup; families?: Lookup }) {
      await seed({ agentProfiles: [PROFILE], agentNotes: [] });
      stageBackend();
      applySettingsSnapshot(makeSettings({ agentProfiles: [PROFILE], agentNotes: [] }), 'laptop');
      const answer = (lookup: Lookup | undefined) => {
        if (lookup === 'rejects') throw new Error('the connection dropped');
        if (lookup === 'older') throw methodNotFound();
      };
      setBindingMock('AgentRosterSeeds', async () => {
        if (takePinnedBackend() === 'laptop') answer(lookups.seeds ?? 'older');
        return SEEDS;
      });
      setBindingMock('CheckAgentProfiles', async (profiles: AgentProfile[]) => {
        const there = takePinnedBackend() === 'laptop';
        answer(there ? lookups.check : lookups.families);
        return profiles.map((profile) =>
          there ? (profile.model === 'claude-opus' ? { missing: LACKS } : {}) : { family: profile.model === 'claude-opus' },
        );
      });
      const view = render(ComputerSettingsPage, { props: { backend: 'laptop', Page: AgentProfilesSettings, needsComputer: false } });
      await settle();
      await fireEvent.change(view.getByLabelText('Provider'), { target: { value: 'claude' } });
      const model = view.getByLabelText('Model') as HTMLInputElement;
      await fireEvent.input(model, { target: { value: 'claude-opus' } });
      await fireEvent.change(model);
      await settle();
      return view;
    }

    const cases: { lookups: { check?: Lookup; seeds?: Lookup; families?: Lookup }; says: string }[] = [
      { lookups: { check: 'rejects' }, says: 'the connection dropped' },
      { lookups: { seeds: 'rejects' }, says: 'the connection dropped' },
      { lookups: { seeds: 'older', families: 'rejects' }, says: 'the connection dropped' },
      { lookups: { seeds: 'older', families: 'older' }, says: 'cannot tell which models are families' },
      { lookups: { check: 'older', families: 'rejects' }, says: 'the connection dropped' },
      { lookups: { check: 'older', families: 'older' }, says: 'cannot tell which models are families' },
    ];
    for (const { lookups, says } of cases) {
      it(`keeps the family unsaved and says why (${JSON.stringify(lookups)})`, async () => {
        try {
          const view = await typeFamily(lookups);
          expect(familyWrites()).toHaveLength(0);
          const failure = view.getByTestId('agent-profiles-check-failure').textContent ?? '';
          expect(failure).toContain('Could not check the profiles against Laptop');
          expect(failure).toContain(says);
          expect((view.getByLabelText('Model') as HTMLInputElement).value).toBe('claude-opus');
        } finally {
          resetStagedBackends();
          resetSettingsForTest();
        }
      });
    }

    it('saves once the check answers again, and stops saying it could not check', async () => {
      try {
        const lookups: { check?: Lookup; seeds?: Lookup } = { check: 'rejects' };
        const view = await typeFamily(lookups);
        expect(view.getByTestId('agent-profiles-check-failure')).toBeTruthy();
        lookups.check = 'answers';
        lookups.seeds = 'answers';
        await fireEvent.change(view.getByLabelText('Model'));
        await settle();
        expect(view.queryByTestId('agent-profiles-check-failure')).toBeNull();
        expect(lastPatch()).toEqual({ agentProfiles: [{ ...PROFILE, provider: 'claude', model: 'claude-opus' }] });
      } finally {
        resetStagedBackends();
        resetSettingsForTest();
      }
    });

    it('holds back a family on a computer with no check, and saves an exact model there', async () => {
      try {
        const view = await typeFamily({ check: 'older', families: 'answers' });
        expect(familyWrites()).toHaveLength(0);
        expect(view.queryByTestId('agent-profiles-check-failure')).toBeNull();
        expect(view.getByTestId('agent-profile-error').textContent).toBe('Laptop runs an older version: it has no model families.');

        const model = view.getByLabelText('Model') as HTMLInputElement;
        await fireEvent.input(model, { target: { value: 'claude-opus-5' } });
        await fireEvent.change(model);
        await settle();
        expect(view.queryByTestId('agent-profile-error')).toBeNull();
        expect(lastPatch()).toEqual({ agentProfiles: [{ ...PROFILE, provider: 'claude', model: 'claude-opus-5' }] });
      } finally {
        resetStagedBackends();
        resetSettingsForTest();
      }
    });
  });

  // v0.0.19's settings name no profiles, notes or levels: the page says
  // the computer runs an older version, offers no editor and checks nothing.
  it('offers no agent profiles on a computer whose release has none', async () => {
    await seed();
    setBindingMock('CheckAgentProfiles', vi.fn(async () => { throw new TransportError('method_not_found', 'unknown method'); }));
    stageBackend();
    try {
      applySettingsSnapshot(settingsAsV0019(), 'laptop');
      const view = render(ComputerSettingsPage, { props: { backend: 'laptop', Page: AgentProfilesSettings, needsComputer: false } });
      expect((await view.findByTestId('settings-older-computer')).textContent).toBe('Laptop runs an older version: it has no agent profiles.');
      await settle();
      const editors = view.container.querySelectorAll('input, textarea, select, button');
      expect(editors, 'an editor for a key the computer lacks').toHaveLength(0);
      expect(getBindingMock('CheckAgentProfiles')).not.toHaveBeenCalled();
      expect(getBindingMock('UpdateSettings')!.mock.calls).toHaveLength(0);
      expect(view.queryByTestId('agent-profile-error')).toBeNull();
    } finally {
      resetStagedBackends();
      resetSettingsForTest();
    }
  });

  describe('each list keeps its own draft', () => {
    it('saving a note leaves an unsaved profile edit alone, and the reverse', async () => {
      await seed({ agentProfiles: [PROFILE], agentNotes: [NOTE] });
      const view = render(AgentProfilesSettings);
      await settle();

      // A profile edit the checks hold back, then a note saved.
      const name = view.getByLabelText('Name') as HTMLInputElement;
      await fireEvent.input(name, { target: { value: '' } });
      await fireEvent.change(name);
      const noteText = view.getByLabelText('Note') as HTMLTextAreaElement;
      await fireEvent.input(noteText, { target: { value: 'Keep it shorter.' } });
      await fireEvent.change(noteText);
      await settle();
      expect(lastPatch()).toEqual({ agentNotes: [{ text: 'Keep it shorter.', when: '' }] });
      expect(name.value).toBe('');
      expect(view.getByTestId('agent-profile-error').textContent).toBe('Give the profile a name.');

      // A note edit held back, then a profile saved.
      await fireEvent.input(noteText, { target: { value: ' ' } });
      await fireEvent.change(noteText);
      await fireEvent.input(name, { target: { value: 'Critic' } });
      await fireEvent.change(name);
      await settle();
      expect(lastPatch()).toEqual({ agentProfiles: [{ ...PROFILE, name: 'Critic' }] });
      expect(noteText.value).toBe(' ');
    });

    it('keeps an unsaved entry when the list changes elsewhere, and takes the rest', async () => {
      const other = { ...PROFILE, name: 'Other', useFor: 'Other things.' };
      await seed({ agentProfiles: [PROFILE, other], agentNotes: [NOTE] });
      const view = render(AgentProfilesSettings);
      await settle();

      const [first] = view.getAllByTestId('agent-profile');
      const useFor = within(first).getByLabelText('Use for') as HTMLTextAreaElement;
      await fireEvent.input(useFor, { target: { value: 'Reviews, half typed' } });
      savedElsewhere({ agentProfiles: [PROFILE, { ...other, useFor: 'Changed there.' }] });
      await settle();

      const [mine, theirs] = view.getAllByTestId('agent-profile');
      expect((within(mine).getByLabelText('Use for') as HTMLTextAreaElement).value).toBe('Reviews, half typed');
      expect((within(theirs).getByLabelText('Use for') as HTMLTextAreaElement).value).toBe('Changed there.');
      expect(view.queryByTestId('agent-profile-conflict')).toBeNull();

      await fireEvent.change(within(mine).getByLabelText('Use for'));
      await settle();
      expect(lastPatch()).toEqual({
        agentProfiles: [{ ...PROFILE, useFor: 'Reviews, half typed' }, { ...other, useFor: 'Changed there.' }],
      });
    });

    // Changed in both places: the person's version stays, marked, until they
    // save it over the other or take the other.
    it('marks an entry changed in both places and lets the person take the other version', async () => {
      await seed({ agentProfiles: [PROFILE], agentNotes: [NOTE] });
      const view = render(AgentProfilesSettings);
      await settle();

      const useFor = view.getByLabelText('Use for') as HTMLTextAreaElement;
      await fireEvent.input(useFor, { target: { value: 'Mine.' } });
      savedElsewhere({ agentProfiles: [{ ...PROFILE, useFor: 'Theirs.' }] });
      await settle();
      expect(useFor.value).toBe('Mine.');
      const conflict = view.getByTestId('agent-profile-conflict');
      expect(conflict.textContent).toContain(
        'This was also changed elsewhere. Your version is kept here and not saved until you choose.',
      );

      await fireEvent.click(within(conflict).getByRole('button', { name: 'Use the other version' }));
      expect((view.getByLabelText('Use for') as HTMLTextAreaElement).value).toBe('Theirs.');
      expect(view.queryByTestId('agent-profile-conflict')).toBeNull();
      expect(getBindingMock('UpdateSettings')!.mock.calls).toHaveLength(0);

      // An edit after it saves as any other.
      await fireEvent.input(view.getByLabelText('Use for'), { target: { value: 'Mine again.' } });
      await fireEvent.change(view.getByLabelText('Use for'));
      await settle();
      expect(lastPatch()).toEqual({ agentProfiles: [{ ...PROFILE, useFor: 'Mine again.' }] });
      expect(view.queryByTestId('agent-profile-conflict')).toBeNull();
    });

    // Leaving a field of a marked entry, by Tab or by pressing one of its
    // buttons, commits the field; it must not save before the person
    // chooses, and the two choices come after the entry's fields.
    it('saves nothing when a marked entry is left, and offers the choices after its fields', async () => {
      await seed({ agentProfiles: [PROFILE], agentNotes: [NOTE] });
      const view = render(AgentProfilesSettings);
      await settle();
      const check = getBindingMock('CheckAgentProfiles')!;

      const useFor = view.getByLabelText('Use for') as HTMLTextAreaElement;
      await fireEvent.input(useFor, { target: { value: 'Mine.' } });
      savedElsewhere({ agentProfiles: [{ ...PROFILE, useFor: 'Theirs.' }] });
      await settle();
      const checks = check.mock.calls.length;
      await fireEvent.change(useFor);
      await fireEvent.change(view.getByLabelText('Effort'), { target: { value: 'medium' } });
      await settle();
      expect(getBindingMock('UpdateSettings')!.mock.calls).toHaveLength(0);
      expect(check.mock.calls).toHaveLength(checks);
      expect(useFor.value).toBe('Mine.');

      const entry = view.getByTestId('agent-profile');
      // Document order is tab order here: nothing in the entry sets tabindex.
      const focusable = [...entry.querySelectorAll<HTMLElement>('input, select, textarea, button')];
      expect(focusable.some((el) => el.hasAttribute('tabindex'))).toBe(false);
      const order = focusable.map((el) =>
        el.tagName === 'BUTTON' ? el.textContent?.trim() : (el.closest('label')?.textContent ?? '').trim() || el.title || el.getAttribute('placeholder'),
      );
      expect(order).toEqual([
        'Name',
        'Provider',
        'A model ID, or a family without a version, such as claude-opus or gpt-sol, which runs the newest model of it a computer lists',
        'Effort',
        'Runtime mode',
        'Delete',
        'Use for',
        'Instructions, appended to the sub-thread\'s system prompt (optional)',
        'Save mine',
        'Use the other version',
      ]);
    });

    it('takes the other version and saves what waited on the choice', async () => {
      const other = { ...PROFILE, name: 'Other', useFor: 'Other things.' };
      await seed({ agentProfiles: [PROFILE, other], agentNotes: [NOTE] });
      const view = render(AgentProfilesSettings);
      await settle();

      const [first, second] = view.getAllByTestId('agent-profile');
      await fireEvent.input(within(first).getByLabelText('Use for'), { target: { value: 'Mine.' } });
      savedElsewhere({ agentProfiles: [{ ...PROFILE, useFor: 'Theirs.' }, other] });
      await settle();
      const otherUseFor = within(second).getByLabelText('Use for');
      await fireEvent.input(otherUseFor, { target: { value: 'Edited while marked.' } });
      await fireEvent.change(otherUseFor);
      await settle();
      expect(getBindingMock('UpdateSettings')!.mock.calls).toHaveLength(0);

      await fireEvent.click(view.getByRole('button', { name: 'Use the other version' }));
      await settle();
      expect(lastPatch()).toEqual({
        agentProfiles: [{ ...PROFILE, useFor: 'Theirs.' }, { ...other, useFor: 'Edited while marked.' }],
      });
    });

    it('saves the person\'s version over the other one when they press Save mine', async () => {
      await seed({ agentProfiles: [PROFILE], agentNotes: [NOTE] });
      const view = render(AgentProfilesSettings);
      await settle();

      const useFor = view.getByLabelText('Use for') as HTMLTextAreaElement;
      await fireEvent.input(useFor, { target: { value: 'Mine.' } });
      savedElsewhere({ agentProfiles: [{ ...PROFILE, useFor: 'Theirs.' }] });
      await settle();
      await fireEvent.change(useFor);
      await settle();
      expect(getBindingMock('UpdateSettings')!.mock.calls).toHaveLength(0);

      await fireEvent.click(view.getByRole('button', { name: 'Save mine' }));
      await settle();
      expect(lastPatch()).toEqual({ agentProfiles: [{ ...PROFILE, useFor: 'Mine.' }] });
      expect(view.queryByTestId('agent-profile-conflict')).toBeNull();
    });

    // A save checks the profiles first; a change saved elsewhere meanwhile
    // is part of what it writes, not overwritten by the list it started with.
    it('writes a change saved elsewhere while its check ran', async () => {
      const other = { ...PROFILE, name: 'Other', useFor: 'Other things.' };
      await seed({ agentProfiles: [PROFILE, other], agentNotes: [NOTE] });
      let release: () => void = () => {};
      setBindingMock('CheckAgentProfiles', (profiles: AgentProfile[]) =>
        new Promise((resolve) => {
          release = () => resolve(profiles.map(() => ({})));
        }),
      );
      const view = render(AgentProfilesSettings);
      await settle();
      release();
      await settle();

      const name = view.getAllByLabelText('Name')[0] as HTMLInputElement;
      await fireEvent.input(name, { target: { value: 'Critic' } });
      await fireEvent.change(name);
      const pending = release;
      savedElsewhere({ agentProfiles: [PROFILE, { ...other, useFor: 'Changed there.' }] });
      await settle();
      release();
      pending();
      await settle();
      release();
      await settle();

      expect(lastPatch()).toEqual({
        agentProfiles: [{ ...PROFILE, name: 'Critic' }, { ...other, useFor: 'Changed there.' }],
      });
    });

    it('keeps a note changed in both places beside the other version', async () => {
      await seed({ agentProfiles: [PROFILE], agentNotes: [NOTE] });
      const view = render(AgentProfilesSettings);
      await settle();

      await fireEvent.input(view.getByLabelText('Note'), { target: { value: 'Mine.' } });
      savedElsewhere({ agentNotes: [{ text: 'Theirs.', when: '' }] });
      await settle();
      const texts = view.getAllByLabelText('Note').map((el) => (el as HTMLTextAreaElement).value);
      expect(texts).toEqual(['Theirs.', 'Mine.']);
      const [, mine] = view.getAllByTestId('agent-note');
      const conflict = within(mine).getByTestId('agent-note-conflict');
      expect(conflict.textContent).toContain(
        'This was changed or removed elsewhere. Your version is kept here and not saved until you choose.',
      );
      await fireEvent.change(view.getAllByLabelText('Note')[1]);
      expect(getBindingMock('UpdateSettings')!.mock.calls).toHaveLength(0);

      await fireEvent.click(within(conflict).getByRole('button', { name: 'Discard mine' }));
      expect(view.getAllByLabelText('Note').map((el) => (el as HTMLTextAreaElement).value)).toEqual(['Theirs.']);
      expect(getBindingMock('UpdateSettings')!.mock.calls).toHaveLength(0);
    });

    it('adds a note changed in both places back when the person saves theirs', async () => {
      await seed({ agentProfiles: [PROFILE], agentNotes: [NOTE] });
      const view = render(AgentProfilesSettings);
      await settle();

      await fireEvent.input(view.getByLabelText('Note'), { target: { value: 'Mine.' } });
      savedElsewhere({ agentNotes: [{ text: 'Theirs.', when: '' }] });
      await settle();
      await fireEvent.click(view.getByRole('button', { name: 'Save mine' }));
      expect(lastPatch()).toEqual({
        agentNotes: [
          { text: 'Theirs.', when: '' },
          { text: 'Mine.', when: '' },
        ],
      });
      expect(view.queryByTestId('agent-note-conflict')).toBeNull();
    });
  });

  describe('in the order things happen', () => {
    /** Each call to `name` waits until the test answers it, in call order. */
    function holdBinding<T>(name: 'CheckAgentProfiles' | 'AgentRosterSeeds') {
      const waiting: { resolve: (value: T) => void; reject: (err: unknown) => void; args: unknown[] }[] = [];
      setBindingMock(name, (...args: unknown[]) => new Promise<T>((resolve, reject) => waiting.push({ resolve, reject, args })));
      return waiting;
    }
    const writes = () => getBindingMock('UpdateSettings')!.mock.calls.map((call) => call[0]);
    const passes = (args: unknown[]) => (args[0] as AgentProfile[]).map(() => ({}));

    // A save that began before Restore and writes while Restore waits does
    // not cancel it: its write is this page's own, and Restore's follows it.
    it('restores after a save that was checking when Restore was clicked writes', async () => {
      await seed({ agentProfiles: [PROFILE], agentNotes: [NOTE] });
      const view = render(AgentProfilesSettings);
      await settle();
      const checks = holdBinding<Check[]>('CheckAgentProfiles');
      const name = view.getByLabelText('Name') as HTMLInputElement;
      await fireEvent.input(name, { target: { value: 'Saved first' } });
      await fireEvent.change(name);
      const seeds = holdBinding<typeof SEEDS>('AgentRosterSeeds');
      await fireEvent.click(view.getByTestId('agent-profiles-restore'));
      await settle();
      checks[0].resolve(passes(checks[0].args));
      await settle();
      seeds[0].resolve(SEEDS);
      await settle();

      expect(writes()).toEqual([
        { agentProfiles: [{ ...PROFILE, name: 'Saved first' }] },
        { agentProfiles: SEEDS.profiles, agentNotes: SEEDS.notes },
      ]);
      expect(view.getAllByTestId('agent-profile').map((row) => row.dataset.profileName)).toEqual(
        SEEDS.profiles.map((profile) => profile.name),
      );
    });

    // Only the newest check's answer shows, whichever answers last.
    it('does not show a failure of a check older than one that answered', async () => {
      await seed({ agentProfiles: [PROFILE], agentNotes: [] });
      const checks = holdBinding<Check[]>('CheckAgentProfiles');
      const view = render(AgentProfilesSettings);
      await settle();
      const name = view.getByLabelText('Name') as HTMLInputElement;
      await fireEvent.input(name, { target: { value: 'Critic' } });
      await fireEvent.change(name);
      await settle();
      checks[1].resolve(passes(checks[1].args));
      await settle();
      checks[0].reject(new Error('the connection dropped'));
      await settle();

      expect(view.queryByTestId('agent-profiles-check-failure')).toBeNull();
      expect(lastPatch()).toEqual({ agentProfiles: [{ ...PROFILE, name: 'Critic' }] });
    });

    it('keeps the failure of the newest check when an older one answers after it', async () => {
      await seed({ agentProfiles: [PROFILE], agentNotes: [] });
      const checks = holdBinding<Check[]>('CheckAgentProfiles');
      const view = render(AgentProfilesSettings);
      await settle();
      const name = view.getByLabelText('Name') as HTMLInputElement;
      await fireEvent.input(name, { target: { value: 'Critic' } });
      await fireEvent.change(name);
      await settle();
      checks[1].reject(new Error('the connection dropped'));
      await settle();
      checks[0].resolve(passes(checks[0].args));
      await settle();

      expect(view.getByTestId('agent-profiles-check-failure').textContent).toContain('the connection dropped');
      expect(getBindingMock('UpdateSettings')!.mock.calls).toHaveLength(0);
    });

    // Deleting, or keeping one's version, is the person's newer action even
    // when the list cannot save yet: a Restore waiting does nothing after it.
    it('retires a waiting Restore when a delete cannot save yet', async () => {
      const other = { ...PROFILE, name: 'Other', useFor: 'Other things.' };
      await seed({ agentProfiles: [PROFILE, other], agentNotes: [NOTE] });
      const view = render(AgentProfilesSettings);
      await settle();
      const [first, second] = view.getAllByTestId('agent-profile');
      await fireEvent.input(within(first).getByLabelText('Name'), { target: { value: '' } });
      const seeds = holdBinding<typeof SEEDS>('AgentRosterSeeds');
      await fireEvent.click(view.getByTestId('agent-profiles-restore'));
      await settle();
      await fireEvent.click(within(second).getByRole('button', { name: 'Delete Other' }));
      seeds[0].resolve(SEEDS);
      await settle();

      expect(getBindingMock('UpdateSettings')!.mock.calls).toHaveLength(0);
      expect(view.getAllByTestId('agent-profile')).toHaveLength(1);
      expect(view.getByTestId('agent-profile-error').textContent).toBe('Give the profile a name.');
    });

    it('retires a waiting Restore when Save mine cannot save yet', async () => {
      const other = { ...PROFILE, name: 'Other', useFor: 'Other things.' };
      await seed({ agentProfiles: [PROFILE, other], agentNotes: [NOTE] });
      const view = render(AgentProfilesSettings);
      await settle();
      for (const row of view.getAllByTestId('agent-profile')) {
        await fireEvent.input(within(row).getByLabelText('Use for'), { target: { value: 'Mine.' } });
      }
      savedElsewhere({ agentProfiles: [{ ...PROFILE, useFor: 'Theirs.' }, { ...other, useFor: 'Theirs too.' }] });
      await settle();
      expect(view.getAllByTestId('agent-profile-conflict')).toHaveLength(2);
      const seeds = holdBinding<typeof SEEDS>('AgentRosterSeeds');
      await fireEvent.click(view.getByTestId('agent-profiles-restore'));
      await settle();
      const [conflict] = view.getAllByTestId('agent-profile-conflict');
      await fireEvent.click(within(conflict).getByRole('button', { name: 'Save mine' }));
      seeds[0].resolve(SEEDS);
      await settle();

      expect(getBindingMock('UpdateSettings')!.mock.calls).toHaveLength(0);
      expect(view.getAllByTestId('agent-profile-conflict')).toHaveLength(1);
      expect(view.getAllByTestId('agent-profile').map((row) => row.dataset.profileName)).toEqual(['Reviewer', 'Other']);
    });

    // The store puts back the saved list when the computer refuses a write;
    // what the person typed stays on the page, and the next commit sends it.
    it('keeps a refused edit on the page and sends it again on the next commit', async () => {
      await seed({ agentProfiles: [PROFILE], agentNotes: [] });
      const keep = getBindingMock('UpdateSettings')!.getMockImplementation()!;
      setBindingMock('UpdateSettings', vi.fn(async () => {
        throw new Error('refused');
      }));
      const view = render(AgentProfilesSettings);
      await settle();
      const name = view.getByLabelText('Name') as HTMLInputElement;
      await fireEvent.input(name, { target: { value: 'Critic' } });
      await fireEvent.change(name);
      await settle();
      expect(getBindingMock('UpdateSettings')!.mock.calls).toHaveLength(1);
      expect(name.value).toBe('Critic');

      setBindingMock('UpdateSettings', vi.fn(keep));
      await fireEvent.change(name);
      await settle();
      expect(lastPatch()).toEqual({ agentProfiles: [{ ...PROFILE, name: 'Critic' }] });
      expect((view.getByLabelText('Name') as HTMLInputElement).value).toBe('Critic');
    });
  });

  it('keeps an emptied roster empty rather than showing the seeds', async () => {
    await seed({ agentProfiles: [], agentNotes: [] });
    const view = render(AgentProfilesSettings);
    await settle();
    expect(view.queryAllByTestId('agent-profile')).toHaveLength(0);
    expect(view.getByText('No profiles. Threads spawn with their own provider and model.')).toBeTruthy();
  });
});

// The page's drafts are the roster editor's state, which it changes only
// through the editor's methods so the editor orders every change. The
// editor's readonly views stop an assignment at type-check, but svelte-check
// accepts a bind: on them and no lint rule knows the editor, so this reads
// the source.
describe('the agent roster page binds nothing', () => {
  it('writes the editor state only through its methods', () => {
    const source = pageSource.replace(/<!--[\s\S]*?-->/g, '');
    expect([...source.matchAll(/\sbind:(?!this\b)[\w$]+/g)].map((m) => m[0].trim())).toEqual([]);
  });
});
