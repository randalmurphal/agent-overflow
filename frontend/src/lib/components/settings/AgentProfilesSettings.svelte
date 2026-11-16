<script lang="ts">
  // The agent roster: the profiles a thread spawns by name, the notes it
  // reads when choosing, and how deep sub-threads may go
  // (internal/settings/agentroster.go). The lists are edited in a local
  // draft and saved whole when a control commits (change, not input), and
  // only when every entry passes the checks the backend makes, so a
  // half-typed name never reaches the agents. An entry that fails a check
  // keeps what was typed and says why beside it.
  import { onMount } from 'svelte';
  import { settingsComputer } from './settingsComputer';
  import SettingsField from './SettingsField.svelte';
  import SettingsHeader from './SettingsHeader.svelte';
  import {
    GHOST_BUTTON_CLASS,
    INPUT_CLASS,
    SECONDARY_BUTTON_CLASS,
    SELECT_CLASS,
  } from './styles';
  import {
    MAX_AGENT_NOTES,
    MAX_AGENT_NOTE_CHARS,
    MAX_AGENT_PROFILES,
    MAX_PROFILE_MODEL_CHARS,
    MAX_PROFILE_NAME_CHARS,
    MAX_PROFILE_USE_FOR_CHARS,
    noteProblems,
    profileProblems,
    providerOffered,
    trimNote,
    trimProfile,
  } from './agentRoster';
  import { SETTINGS_DEFAULTS } from '../../generated/settingsDefaults';
  import { GetProviderStatuses } from '../../stores/bindings';
  import { addErrorToast } from '../../stores/toast.svelte';
  import type { AgentNote, AgentNoteWhen, AgentProfile, ProviderStatus } from '../../types/settings';

  const { getSettings, updateSetting, updateSettingsPatch, call } = settingsComputer();
  let settings = $derived(getSettings());

  const PROVIDERS: { value: AgentProfile['provider']; label: string }[] = [
    { value: '', label: 'Inherit' },
    { value: 'claude', label: 'Claude' },
    { value: 'codex', label: 'Codex' },
  ];
  const PROVIDER_NAMES = { claude: 'Claude', codex: 'Codex' } as const;
  const EFFORTS: { value: AgentProfile['effort']; label: string }[] = [
    { value: '', label: 'Inherit' },
    ...(['none', 'minimal', 'low', 'medium', 'high', 'xhigh', 'max', 'ultra'] as const).map((value) => ({
      value,
      label: value,
    })),
  ];
  const MODES: { value: AgentProfile['runtimeMode']; label: string }[] = [
    { value: '', label: 'Inherit' },
    { value: 'read-only', label: 'Read-only' },
    { value: 'approval-required', label: 'Supervised' },
    { value: 'auto-accept-edits', label: 'Auto-accept edits' },
    { value: 'auto', label: 'Auto' },
    { value: 'full-access', label: 'Full access' },
  ];
  const NOTE_WHENS: { value: AgentNoteWhen; label: string }[] = [
    { value: '', label: 'Always' },
    { value: 'both_providers', label: 'When Claude and Codex both run here' },
    { value: 'one_provider', label: 'When only one of them runs here' },
  ];
  const LEVELS = [1, 2, 3, 4];

  function cloneProfiles(list: readonly AgentProfile[]): AgentProfile[] {
    return list.map((profile) => ({ ...profile }));
  }
  function cloneNotes(list: readonly AgentNote[]): AgentNote[] {
    return list.map((note) => ({ ...note }));
  }

  // The draft follows the lists when they change: a save from here (the
  // store shows it at once, so its reply changes nothing when the backend
  // takes it, and puts back the saved lists with its error toast when it
  // does not) or one made elsewhere.
  let profiles = $state<AgentProfile[]>(cloneProfiles(getSettings().agentProfiles));
  let notes = $state<AgentNote[]>(cloneNotes(getSettings().agentNotes));
  let followed = JSON.stringify([getSettings().agentProfiles, getSettings().agentNotes]);
  $effect(() => {
    const saved = JSON.stringify([settings.agentProfiles, settings.agentNotes]);
    if (saved === followed) return;
    followed = saved;
    profiles = cloneProfiles(settings.agentProfiles);
    notes = cloneNotes(settings.agentNotes);
  });

  let profileErrors = $derived(profileProblems(profiles));
  let noteErrors = $derived(noteProblems(notes));

  function saveProfiles(): void {
    if (profileErrors.size > 0) return;
    const trimmed = profiles.map(trimProfile);
    void updateSetting('agentProfiles', trimmed);
  }
  function saveNotes(): void {
    if (noteErrors.size > 0) return;
    const trimmed = notes.map(trimNote);
    void updateSetting('agentNotes', trimmed);
  }

  function addProfile(): void {
    profiles.push({ name: '', useFor: '', provider: '', model: '', effort: '', runtimeMode: '', instructions: '' });
  }
  function deleteProfile(index: number): void {
    profiles.splice(index, 1);
    saveProfiles();
  }
  function addNote(): void {
    notes.push({ text: '', when: '' });
  }
  function deleteNote(index: number): void {
    notes.splice(index, 1);
    saveNotes();
  }
  function restore(): void {
    profiles = cloneProfiles(SETTINGS_DEFAULTS.agentProfiles as AgentProfile[]);
    notes = cloneNotes(SETTINGS_DEFAULTS.agentNotes as AgentNote[]);
    void updateSettingsPatch({ agentProfiles: cloneProfiles(profiles), agentNotes: cloneNotes(notes) });
  }

  // Which providers run here, for marking a profile that is not offered.
  let statuses = $state<ProviderStatus[] | undefined>(undefined);
  onMount(() => {
    let live = true;
    void (async () => {
      try {
        const all = ((await call(() => GetProviderStatuses())) ?? []) as ProviderStatus[];
        if (live) statuses = all;
      } catch (err) {
        if (!live) return;
        console.error('Failed to load provider statuses:', err);
        addErrorToast('Failed to load provider statuses.', err);
      }
    })();
    return () => {
      live = false;
    };
  });
</script>

<div class="settings-sections" data-testid="settings-agent-profiles">
  <section>
    <div class="flex flex-col gap-1">
      <SettingsField
        id="agent-profiles.levels"
        label="Sub-thread levels"
        hint="How many levels of sub-threads may open below a thread you talk to. A thread on the last level cannot open threads and has no subagents of its own. Applies to sessions started after a change."
        htmlFor="agent-profiles-levels"
      >
        <select
          id="agent-profiles-levels"
          data-testid="agent-profiles-levels"
          value={settings.subThreadLevels}
          onchange={(e) => void updateSetting('subThreadLevels', Number((e.target as HTMLSelectElement).value))}
          class={SELECT_CLASS}
        >
          {#each LEVELS as level (level)}
            <option value={level}>{level}</option>
          {/each}
        </select>
      </SettingsField>
    </div>
  </section>

  <section>
    <SettingsHeader
      title="Profiles"
      description="Agents a thread can open a sub-thread with by name, choosing by what each is for. Inherit takes the spawning thread's own choice."
    />
    <div
      class="flex flex-col gap-3"
      data-settings-field="agent-profiles.profiles"
      data-settings-label="Profiles"
      data-testid="agent-profiles-list"
    >
      {#if profiles.length === 0}
        <p class="text-[0.71875rem] text-fg-hint">No profiles. Threads spawn with their own provider and model.</p>
      {/if}
      {#each profiles as profile, index (index)}
        {@const offered = providerOffered(profile.provider, settings, statuses)}
        {@const error = profileErrors.get(index)}
        <div
          class="flex flex-col gap-2 rounded-[var(--radius-field)] border border-border-subtle bg-surface-1/30 p-3"
          data-testid="agent-profile"
          data-profile-name={profile.name}
        >
          <div class="flex flex-wrap items-center gap-2">
            <label class="sr-only" for="agent-profile-name-{index}">Name</label>
            <input
              id="agent-profile-name-{index}"
              type="text"
              placeholder="Name"
              maxlength={MAX_PROFILE_NAME_CHARS}
              bind:value={profile.name}
              onchange={saveProfiles}
              aria-invalid={error !== undefined}
              class="{INPUT_CLASS} max-w-[10rem] font-medium"
            />
            <label class="sr-only" for="agent-profile-provider-{index}">Provider</label>
            <select
              id="agent-profile-provider-{index}"
              title="Provider"
              value={profile.provider}
              onchange={(e) => {
                profile.provider = (e.target as HTMLSelectElement).value as AgentProfile['provider'];
                saveProfiles();
              }}
              class={SELECT_CLASS}
            >
              {#each PROVIDERS as option (option.value)}
                <option value={option.value}>{option.value === '' ? 'Inherit provider' : option.label}</option>
              {/each}
            </select>
            <label class="sr-only" for="agent-profile-model-{index}">Model</label>
            <input
              id="agent-profile-model-{index}"
              type="text"
              placeholder="Inherit model"
              maxlength={MAX_PROFILE_MODEL_CHARS}
              bind:value={profile.model}
              onchange={saveProfiles}
              class="{INPUT_CLASS} max-w-[12rem] font-mono"
            />
            <label class="sr-only" for="agent-profile-effort-{index}">Effort</label>
            <select
              id="agent-profile-effort-{index}"
              title="Effort"
              value={profile.effort}
              onchange={(e) => {
                profile.effort = (e.target as HTMLSelectElement).value as AgentProfile['effort'];
                saveProfiles();
              }}
              class={SELECT_CLASS}
            >
              {#each EFFORTS as option (option.value)}
                <option value={option.value}>{option.value === '' ? 'Inherit effort' : option.label}</option>
              {/each}
            </select>
            <label class="sr-only" for="agent-profile-mode-{index}">Runtime mode</label>
            <select
              id="agent-profile-mode-{index}"
              title="Runtime mode"
              value={profile.runtimeMode}
              onchange={(e) => {
                profile.runtimeMode = (e.target as HTMLSelectElement).value as AgentProfile['runtimeMode'];
                saveProfiles();
              }}
              class={SELECT_CLASS}
            >
              {#each MODES as option (option.value)}
                <option value={option.value}>{option.value === '' ? 'Inherit mode' : option.label}</option>
              {/each}
            </select>
            <button
              type="button"
              class="{GHOST_BUTTON_CLASS} ml-auto"
              aria-label={`Delete ${profile.name || 'this profile'}`}
              onclick={() => deleteProfile(index)}
            >
              Delete
            </button>
          </div>
          {#if offered === false && profile.provider !== ''}
            <p class="text-[0.71875rem] text-fg-muted" data-testid="agent-profile-unavailable">
              {`${PROVIDER_NAMES[profile.provider]} does not run on this computer, so threads here are not offered this profile. A paired computer that runs it still offers it.`}
            </p>
          {/if}
          <label class="flex flex-col gap-1 text-[0.71875rem] text-fg-muted">
            Use for
            <textarea
              rows={2}
              maxlength={MAX_PROFILE_USE_FOR_CHARS}
              placeholder="What this agent is good at, what it costs, and what not to use it for."
              bind:value={profile.useFor}
              onchange={saveProfiles}
              class="{INPUT_CLASS} resize-y"
            ></textarea>
          </label>
          <label class="flex flex-col gap-1 text-[0.71875rem] text-fg-muted">
            Instructions, appended to the sub-thread's system prompt (optional)
            <textarea
              rows={2}
              bind:value={profile.instructions}
              onchange={saveProfiles}
              class="{INPUT_CLASS} resize-y"
            ></textarea>
          </label>
          {#if error}
            <p class="text-[0.71875rem] text-error" role="alert" data-testid="agent-profile-error">{error}</p>
          {/if}
        </div>
      {/each}
      <div>
        <button
          type="button"
          class={SECONDARY_BUTTON_CLASS}
          disabled={profiles.length >= MAX_AGENT_PROFILES}
          onclick={addProfile}
        >
          Add profile
        </button>
      </div>
    </div>
  </section>

  <section>
    <SettingsHeader
      title="Choosing notes"
      description="Advice a thread reads with the profiles when it picks one. A note can be shown only when both Claude and Codex run on the computer, or only when one of them does."
    />
    <div
      class="flex flex-col gap-3"
      data-settings-field="agent-profiles.notes"
      data-settings-label="Choosing notes"
      data-testid="agent-notes-list"
    >
      {#each notes as note, index (index)}
        {@const error = noteErrors.get(index)}
        <div class="flex flex-col gap-2" data-testid="agent-note">
          <label class="sr-only" for="agent-note-text-{index}">Note</label>
          <textarea
            id="agent-note-text-{index}"
            rows={3}
            maxlength={MAX_AGENT_NOTE_CHARS}
            bind:value={note.text}
            onchange={saveNotes}
            aria-invalid={error !== undefined}
            class="{INPUT_CLASS} resize-y"
          ></textarea>
          <div class="flex min-w-0 items-center gap-2">
            <label class="shrink-0 text-[0.71875rem] text-fg-muted" for="agent-note-when-{index}">Shown</label>
            <select
              id="agent-note-when-{index}"
              value={note.when}
              onchange={(e) => {
                note.when = (e.target as HTMLSelectElement).value as AgentNoteWhen;
                saveNotes();
              }}
              class="{SELECT_CLASS} min-w-0 flex-1 truncate"
            >
              {#each NOTE_WHENS as option (option.value)}
                <option value={option.value}>{option.label}</option>
              {/each}
            </select>
            <button type="button" class="{GHOST_BUTTON_CLASS} ml-auto" onclick={() => deleteNote(index)}>
              Delete
            </button>
          </div>
          {#if error}
            <p class="text-[0.71875rem] text-error" role="alert">{error}</p>
          {/if}
        </div>
      {/each}
      <div>
        <button
          type="button"
          class={SECONDARY_BUTTON_CLASS}
          disabled={notes.length >= MAX_AGENT_NOTES}
          onclick={addNote}
        >
          Add note
        </button>
      </div>
    </div>
  </section>

  <section>
    <div class="flex flex-col gap-1">
      <SettingsField
        id="agent-profiles.restore"
        label="Restore the starting roster"
        hint="Replace the profiles and notes with the ones the app ships with. Your edits to them are lost."
      >
        <button type="button" class={SECONDARY_BUTTON_CLASS} onclick={restore} data-testid="agent-profiles-restore">
          Restore
        </button>
      </SettingsField>
    </div>
  </section>
</div>
