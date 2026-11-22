<script lang="ts">
  // The agent roster: the profiles a thread spawns by name, the notes it
  // reads when choosing, and how deep sub-threads may go
  // (internal/settings/agentroster.go). Each list is edited in its own
  // draft (rosterDraft.ts) and saved whole when a control commits (change,
  // not input), and only when every entry passes the checks the backend
  // makes, so a half-typed name never reaches the agents. An entry that
  // fails a check keeps what was typed and says why beside it. While an
  // entry is marked as changed elsewhere too, its list is not saved until
  // the person answers the mark with Save mine or the other version. A
  // computer on a release before the roster gets none of it.
  import { onMount, untrack } from 'svelte';
  import { settingsComputer } from './settingsComputer';
  import { computerHasSetting } from '../../stores/settings.svelte';
  import SettingsOlderComputer from './SettingsOlderComputer.svelte';
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
    trimNote,
    trimProfile,
  } from './agentRoster';
  import {
    DraftRevision,
    draftOf,
    keepMine,
    markSaved,
    marked,
    reconcile,
    takeTheirs,
    unsaved,
    type Draft,
  } from './rosterDraft';
  import { SETTINGS_DEFAULTS } from '../../generated/settingsDefaults';
  import { CheckAgentProfiles } from '../../stores/bindings';
  import type { AgentNote, AgentNoteWhen, AgentProfile } from '../../types/settings';

  const { backend, getSettings, updateSetting, updateSettingsPatch, call } = settingsComputer();
  let settings = $derived(getSettings());
  const KEYS = ['agentProfiles', 'agentNotes', 'subThreadLevels'] as const;
  let older = $derived(!KEYS.every((key) => computerHasSetting(key, backend)));

  const PROVIDERS: { value: AgentProfile['provider']; label: string }[] = [
    { value: '', label: 'Inherit' },
    { value: 'claude', label: 'Claude' },
    { value: 'codex', label: 'Codex' },
    { value: 'agy', label: 'Antigravity' },
  ];
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

  // A profile's saved counterpart, when it changed elsewhere too: the one
  // with its name, which is unique in the list.
  const sameProfile = (origin: AgentProfile, entry: AgentProfile) =>
    origin.name.toLowerCase() === entry.name.toLowerCase();

  // Each list follows its own saved value: a save from here (the store
  // shows it at once, and puts back the saved list with its error toast
  // when the backend refuses it) or one made elsewhere. What is dirty here
  // stays (rosterDraft.ts reconcile).
  let profiles = $state<Draft<AgentProfile>>(draftOf(getSettings().agentProfiles));
  let notes = $state<Draft<AgentNote>>(draftOf(getSettings().agentNotes));
  let followedProfiles = JSON.stringify(getSettings().agentProfiles);
  let followedNotes = JSON.stringify(getSettings().agentNotes);
  const profilesRevision = new DraftRevision();
  const notesRevision = new DraftRevision();
  $effect(() => {
    const saved = settings.agentProfiles;
    const key = JSON.stringify(saved);
    if (key === followedProfiles) return;
    followedProfiles = key;
    untrack(() => {
      const dropped = profilesRevision.replace();
      profiles = reconcile($state.snapshot(profiles) as Draft<AgentProfile>, saved, trimProfile, sameProfile);
      void checkProfiles(saved.map(trimProfile));
      // The save the change dropped writes the reconciled draft instead.
      if (dropped) void saveProfiles();
    });
  });
  $effect(() => {
    const saved = settings.agentNotes;
    const key = JSON.stringify(saved);
    if (key === followedNotes) return;
    followedNotes = key;
    untrack(() => {
      notesRevision.replace();
      notes = reconcile($state.snapshot(notes) as Draft<AgentNote>, saved, trimNote);
    });
  });

  let profileErrors = $derived(profileProblems(profiles.entries.map((entry) => entry.value)));
  let noteErrors = $derived(noteProblems(notes.entries.map((entry) => entry.value)));

  // What this computer makes of each profile's provider, model and effort
  // (CheckAgentProfiles): a problem blocks the save, a missing provider or
  // model is a note. Keyed by those three, so an entry shows the check of
  // what it holds and nothing while that is unchecked.
  type ProfileCheck = { problem?: string; missing?: string };
  let checks = $state<Record<string, ProfileCheck>>({});
  const checkKey = (profile: AgentProfile) => JSON.stringify([profile.provider, profile.model.trim(), profile.effort]);

  /**
   * Checks `list` and records the results; false when a profile cannot
   * run, null when the computer could not check (an older one, or the call
   * failed), so the save goes ahead and UpdateSettings decides.
   */
  async function checkProfiles(list: AgentProfile[]): Promise<boolean | null> {
    let results: ProfileCheck[];
    try {
      const answer = await call(() => CheckAgentProfiles(list));
      // An older computer has no check (bindings.ts, CheckAgentProfiles).
      if (answer === null) return null;
      results = answer as ProfileCheck[];
    } catch (err) {
      console.warn('Could not check the agent profiles against this computer:', err);
      return null;
    }
    const next = { ...checks };
    list.forEach((profile, index) => {
      next[checkKey(profile)] = results[index] ?? {};
    });
    checks = next;
    return results.every((result) => !result?.problem);
  }

  // A list with an entry marked as changed elsewhere too is not saved: a
  // field the person leaves keeps its draft, and the save waits for them
  // to keep theirs or take the other version on every marked entry.
  async function saveProfiles(): Promise<void> {
    if (profileErrors.size > 0 || marked(profiles)) return;
    const revision = profilesRevision.beginSave();
    const entries = [...profiles.entries];
    const sent = entries.map((entry) => trimProfile($state.snapshot(entry.value) as AgentProfile));
    const runnable = await checkProfiles(sent);
    if (!profilesRevision.finishSave(revision) || runnable === false) return;
    markSaved(profiles, entries, sent);
    void updateSetting('agentProfiles', sent);
  }
  function saveNotes(): void {
    if (noteErrors.size > 0 || marked(notes)) return;
    const revision = notesRevision.beginSave();
    const entries = [...notes.entries];
    const sent = entries.map((entry) => trimNote($state.snapshot(entry.value) as AgentNote));
    if (!notesRevision.finishSave(revision)) return;
    markSaved(notes, entries, sent);
    void updateSetting('agentNotes', sent);
  }

  // The two answers to a mark. Taking the other version replaces the draft,
  // dropping a save still checking it; either one saves what the list
  // holds unsaved once nothing marked is left.
  function keepMineProfile(index: number): void {
    keepMine(profiles, index);
    void saveProfiles();
  }
  function takeTheirsProfile(index: number): void {
    profilesRevision.replace();
    takeTheirs(profiles, index);
    if (unsaved(profiles, trimProfile)) void saveProfiles();
  }
  function keepMineNote(index: number): void {
    keepMine(notes, index);
    saveNotes();
  }
  function takeTheirsNote(index: number): void {
    notesRevision.replace();
    takeTheirs(notes, index);
    if (unsaved(notes, trimNote)) saveNotes();
  }
  function addProfile(): void {
    profiles.entries.push({
      value: { name: '', useFor: '', provider: '', model: '', effort: '', runtimeMode: '', instructions: '' },
      origin: null,
    });
  }
  function deleteProfile(index: number): void {
    const [entry] = profiles.entries.splice(index, 1);
    if (entry?.origin) profiles.removed.push(entry.origin);
    void saveProfiles();
  }
  function addNote(): void {
    notes.entries.push({ value: { text: '', when: '' }, origin: null });
  }
  function deleteNote(index: number): void {
    const [entry] = notes.entries.splice(index, 1);
    if (entry?.origin) notes.removed.push(entry.origin);
    saveNotes();
  }
  function restore(): void {
    const shippedProfiles = SETTINGS_DEFAULTS.agentProfiles as AgentProfile[];
    const shippedNotes = SETTINGS_DEFAULTS.agentNotes as AgentNote[];
    profilesRevision.replace();
    notesRevision.replace();
    profiles = draftOf(shippedProfiles);
    notes = draftOf(shippedNotes);
    void updateSettingsPatch({
      agentProfiles: shippedProfiles.map((profile) => ({ ...profile })),
      agentNotes: shippedNotes.map((note) => ({ ...note })),
    });
  }

  onMount(() => {
    if (!older) void checkProfiles(getSettings().agentProfiles.map(trimProfile));
  });
</script>

<!--
  An entry changed here while the saved list changed it too: the person's
  version stays, unsaved, until they save it over the other or take the
  other. It follows the entry's fields, so the keyboard reaches it after
  them.
-->
{#snippet changedElsewhere(testId: string, replaced: boolean, saveMine: () => void, takeOther: () => void)}
  <p class="flex flex-wrap items-baseline gap-x-2 text-[0.71875rem] text-fg-muted" role="status" data-testid={testId}>
    <span>
      {replaced
        ? 'This was also changed elsewhere. Your version is kept here and not saved until you choose.'
        : 'This was changed or removed elsewhere. Your version is kept here and not saved until you choose.'}
    </span>
    <button type="button" class={GHOST_BUTTON_CLASS} onclick={saveMine}>Save mine</button>
    <button type="button" class={GHOST_BUTTON_CLASS} onclick={takeOther}>
      {replaced ? 'Use the other version' : 'Discard mine'}
    </button>
  </p>
{/snippet}

<div class="settings-sections" data-testid="settings-agent-profiles">
  {#if older}
  <section>
    <SettingsOlderComputer {backend} lacks="agent profiles" />
  </section>
  {:else}
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
      {#if profiles.entries.length === 0}
        <p class="text-[0.71875rem] text-fg-hint">No profiles. Threads spawn with their own provider and model.</p>
      {/if}
      {#each profiles.entries as entry, index (index)}
        {@const profile = entry.value}
        {@const check = checks[checkKey(profile)]}
        {@const error = profileErrors.get(index) ?? check?.problem}
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
              bind:value={entry.value.name}
              onchange={() => void saveProfiles()}
              aria-invalid={error !== undefined}
              class="{INPUT_CLASS} max-w-[10rem] font-medium"
            />
            <label class="sr-only" for="agent-profile-provider-{index}">Provider</label>
            <select
              id="agent-profile-provider-{index}"
              title="Provider"
              value={profile.provider}
              onchange={(e) => {
                entry.value.provider = (e.target as HTMLSelectElement).value as AgentProfile['provider'];
                void saveProfiles();
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
              bind:value={entry.value.model}
              onchange={() => void saveProfiles()}
              class="{INPUT_CLASS} max-w-[12rem] font-mono"
            />
            <label class="sr-only" for="agent-profile-effort-{index}">Effort</label>
            <select
              id="agent-profile-effort-{index}"
              title="Effort"
              value={profile.effort}
              onchange={(e) => {
                entry.value.effort = (e.target as HTMLSelectElement).value as AgentProfile['effort'];
                void saveProfiles();
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
                entry.value.runtimeMode = (e.target as HTMLSelectElement).value as AgentProfile['runtimeMode'];
                void saveProfiles();
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
          {#if check?.missing && !error}
            <p class="text-[0.71875rem] text-fg-muted" data-testid="agent-profile-unavailable">
              {`${check.missing} Threads here are not offered this profile; a paired computer that runs it still offers it.`}
            </p>
          {/if}
          <label class="flex flex-col gap-1 text-[0.71875rem] text-fg-muted">
            Use for
            <textarea
              rows={2}
              maxlength={MAX_PROFILE_USE_FOR_CHARS}
              placeholder="What this agent is good at, what it costs, and what not to use it for."
              bind:value={entry.value.useFor}
              onchange={() => void saveProfiles()}
              class="{INPUT_CLASS} resize-y"
            ></textarea>
          </label>
          <label class="flex flex-col gap-1 text-[0.71875rem] text-fg-muted">
            Instructions, appended to the sub-thread's system prompt (optional)
            <textarea
              rows={2}
              bind:value={entry.value.instructions}
              onchange={() => void saveProfiles()}
              class="{INPUT_CLASS} resize-y"
            ></textarea>
          </label>
          {#if entry.theirs !== undefined}
            {@render changedElsewhere(
              'agent-profile-conflict',
              entry.theirs !== null,
              () => keepMineProfile(index),
              () => takeTheirsProfile(index),
            )}
          {/if}
          {#if error}
            <p class="text-[0.71875rem] text-error" role="alert" data-testid="agent-profile-error">{error}</p>
          {/if}
        </div>
      {/each}
      <div>
        <button
          type="button"
          class={SECONDARY_BUTTON_CLASS}
          disabled={profiles.entries.length >= MAX_AGENT_PROFILES}
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
      {#each notes.entries as entry, index (index)}
        {@const note = entry.value}
        {@const error = noteErrors.get(index)}
        <div class="flex flex-col gap-2" data-testid="agent-note">
          <label class="sr-only" for="agent-note-text-{index}">Note</label>
          <textarea
            id="agent-note-text-{index}"
            rows={3}
            maxlength={MAX_AGENT_NOTE_CHARS}
            bind:value={entry.value.text}
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
                entry.value.when = (e.target as HTMLSelectElement).value as AgentNoteWhen;
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
          {#if entry.theirs !== undefined}
            {@render changedElsewhere(
              'agent-note-conflict',
              entry.theirs !== null,
              () => keepMineNote(index),
              () => takeTheirsNote(index),
            )}
          {/if}
          {#if error}
            <p class="text-[0.71875rem] text-error" role="alert">{error}</p>
          {/if}
        </div>
      {/each}
      <div>
        <button
          type="button"
          class={SECONDARY_BUTTON_CLASS}
          disabled={notes.entries.length >= MAX_AGENT_NOTES}
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
  {/if}
</div>
