<script lang="ts">
  // The agent roster: the profiles a thread spawns by name, the notes it
  // reads when choosing, and how deep sub-threads may go
  // (internal/settings/agentroster.go). Each list is edited in its own
  // draft and saved whole when a control commits (change,
  // not input), and only when every entry passes the checks the backend
  // makes, so a half-typed name never reaches the agents. An entry that
  // fails a check keeps what was typed and says why beside it. While an
  // entry is marked as changed elsewhere too, its list is not saved until
  // the person answers the mark with Save mine or the other version. A
  // computer on a release before the roster gets none of it. Every change
  // to the drafts, the checks and Restore goes through one owner
  // (rosterEditor.svelte.ts), which orders them.
  import { onMount, untrack } from 'svelte';
  import { settingsComputer } from './settingsComputer';
  import { computerHasSetting, olderComputerNote, settingsComputerName } from '../../stores/settings.svelte';
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
  } from './agentRoster';
  import { RosterEditor, checkKey, type ProfileCheck, type Roster } from './rosterEditor.svelte';
  import { AgentRosterSeeds, CheckAgentProfiles } from '../../stores/bindings';
  import { addErrorToast } from '../../stores/toast.svelte';
  import type { AgentNoteWhen, AgentProfile } from '../../types/settings';

  const { backend, getSettings, updateSetting, updateSettingsPatch, call } = settingsComputer();
  let settings = $derived(getSettings());
  const KEYS = ['agentProfiles', 'agentNotes', 'subThreadLevels'] as const;
  let older = $derived(!KEYS.every((key) => computerHasSetting(key, backend)));

  const PROVIDERS: { value: AgentProfile['provider']; label: string }[] = [
    { value: '', label: 'Inherit' },
    { value: 'claude', label: 'Claude' },
    { value: 'codex', label: 'Codex' },
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

  /**
   * What this computer makes of each profile, with a problem on each
   * family it cannot run because it predates model families: one with no
   * check of its own (CheckAgentProfiles answers null), or one whose check
   * finds the family missing and that has no AgentRosterSeeds. Such a
   * computer reads a family as an exact model ID it does not offer. Which
   * models are families is read from this window's own computer, which
   * owns the version rules; a profile the computer runs as written is left
   * alone. Throws when a lookup the answer needs fails or has no answer.
   */
  async function profileChecks(list: AgentProfile[]): Promise<ProfileCheck[]> {
    const there = (await call(() => CheckAgentProfiles(list))) as ProfileCheck[] | null;
    if (there !== null && !there.some((result) => result?.missing)) return there;
    if (there !== null && (await call(() => AgentRosterSeeds())) !== null) return there;
    const families = (await CheckAgentProfiles(list)) as ProfileCheck[] | null;
    if (families === null) throw new Error("this window's computer cannot tell which models are families.");
    const lacks = olderComputerNote(backend, 'model families');
    return list.map((_, index) => {
      const result = there?.[index] ?? {};
      if (!families[index]?.family) return result;
      if (there === null) return { ...result, problem: lacks };
      return result.missing ? { ...result, problem: `${lacks} ${result.missing}` } : result;
    });
  }

  const editor = new RosterEditor({
    saved: () => getSettings(),
    check: profileChecks,
    checkFailure: (err) => {
      const reason = err instanceof Error ? err.message : String(err);
      return `Could not check the profiles against ${settingsComputerName(backend) || 'this computer'}: ${reason} A change to them is not saved until the check answers.`;
    },
    seeds: async () => (await call(() => AgentRosterSeeds())) as Roster | null,
    noSeeds: () => olderComputerNote(backend, 'starting roster of its own to restore'),
    seedsFailed: (err) => addErrorToast('Could not read the starting roster', err),
    write: (patch) => updateSettingsPatch(patch),
  });

  // Each list follows its own saved value: a save from here, a save made
  // elsewhere, or the store putting back the saved list after it refused
  // one. The owner tells this page's own write apart.
  $effect(() => {
    const saved = settings.agentProfiles;
    untrack(() => editor.follow('profiles', saved));
  });
  $effect(() => {
    const saved = settings.agentNotes;
    untrack(() => editor.follow('notes', saved));
  });

  let profileErrors = $derived(profileProblems(editor.profiles.entries.map((entry) => entry.value)));
  let noteErrors = $derived(noteProblems(editor.notes.entries.map((entry) => entry.value)));

  const inputValue = (e: Event) => (e.target as HTMLInputElement | HTMLTextAreaElement | HTMLSelectElement).value;
  function chooseProfile(index: number, patch: Partial<AgentProfile>): void {
    editor.editProfile(index, patch);
    editor.commit('profiles');
  }

  onMount(() => {
    if (!older) editor.mount();
    return () => editor.close();
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
      {#if editor.checkFailure}
        <p class="text-[0.71875rem] text-error" role="alert" data-testid="agent-profiles-check-failure">{editor.checkFailure}</p>
      {/if}
      {#if editor.profiles.entries.length === 0}
        <p class="text-[0.71875rem] text-fg-hint">No profiles. Threads spawn with their own provider and model.</p>
      {/if}
      {#each editor.profiles.entries as entry, index (index)}
        {@const profile = entry.value}
        {@const check = editor.checks[checkKey(profile)]}
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
              value={profile.name}
              oninput={(e) => editor.editProfile(index, { name: inputValue(e) })}
              onchange={() => editor.commit('profiles')}
              aria-invalid={error !== undefined}
              class="{INPUT_CLASS} max-w-[10rem] font-medium"
            />
            <label class="sr-only" for="agent-profile-provider-{index}">Provider</label>
            <select
              id="agent-profile-provider-{index}"
              title="Provider"
              value={profile.provider}
              onchange={(e) => chooseProfile(index, { provider: inputValue(e) as AgentProfile['provider'] })}
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
              title="A model ID, or a family without a version, such as claude-opus or gpt-sol, which runs the newest model of it a computer lists"
              maxlength={MAX_PROFILE_MODEL_CHARS}
              value={profile.model}
              oninput={(e) => editor.editProfile(index, { model: inputValue(e) })}
              onchange={() => editor.commit('profiles')}
              class="{INPUT_CLASS} max-w-[12rem] font-mono"
            />
            <label class="sr-only" for="agent-profile-effort-{index}">Effort</label>
            <select
              id="agent-profile-effort-{index}"
              title="Effort"
              value={profile.effort}
              onchange={(e) => chooseProfile(index, { effort: inputValue(e) as AgentProfile['effort'] })}
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
              onchange={(e) => chooseProfile(index, { runtimeMode: inputValue(e) as AgentProfile['runtimeMode'] })}
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
              onclick={() => editor.remove('profiles', index)}
            >
              Delete
            </button>
          </div>
          {#if check?.missing && !error}
            <p class="text-[0.71875rem] text-fg-muted" data-testid="agent-profile-unavailable">
              {`${check.missing} Threads here are not offered this profile; a paired computer that runs it still offers it.`}
            </p>
          {/if}
          {#if check?.runs && check.runs !== profile.model.trim() && !error}
            <p class="text-[0.71875rem] text-fg-muted" data-testid="agent-profile-runs">
              {`Runs ${check.runs} here, the newest ${profile.model.trim()} model this computer lists.`}
            </p>
          {/if}
          {#if check?.unreadable?.length && !error}
            <p class="text-[0.71875rem] text-fg-muted" data-testid="agent-profile-unreadable">
              {`Also listed here, but never run for a family: ${check.unreadable.join(', ')}. Agent Overflow cannot read their version.`}
            </p>
          {/if}
          <label class="flex flex-col gap-1 text-[0.71875rem] text-fg-muted">
            Use for
            <textarea
              rows={2}
              maxlength={MAX_PROFILE_USE_FOR_CHARS}
              placeholder="What this agent is good at, what it costs, and what not to use it for."
              value={profile.useFor}
              oninput={(e) => editor.editProfile(index, { useFor: inputValue(e) })}
              onchange={() => editor.commit('profiles')}
              class="{INPUT_CLASS} resize-y"
            ></textarea>
          </label>
          <label class="flex flex-col gap-1 text-[0.71875rem] text-fg-muted">
            Instructions, appended to the sub-thread's system prompt (optional)
            <textarea
              rows={2}
              value={profile.instructions}
              oninput={(e) => editor.editProfile(index, { instructions: inputValue(e) })}
              onchange={() => editor.commit('profiles')}
              class="{INPUT_CLASS} resize-y"
            ></textarea>
          </label>
          {#if entry.theirs !== undefined}
            {@render changedElsewhere(
              'agent-profile-conflict',
              entry.theirs !== null,
              () => editor.keepMine('profiles', index),
              () => editor.takeTheirs('profiles', index),
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
          disabled={editor.profiles.entries.length >= MAX_AGENT_PROFILES}
          onclick={() => editor.addProfile()}
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
      {#each editor.notes.entries as entry, index (index)}
        {@const note = entry.value}
        {@const error = noteErrors.get(index)}
        <div class="flex flex-col gap-2" data-testid="agent-note">
          <label class="sr-only" for="agent-note-text-{index}">Note</label>
          <textarea
            id="agent-note-text-{index}"
            rows={3}
            maxlength={MAX_AGENT_NOTE_CHARS}
            value={note.text}
            oninput={(e) => editor.editNote(index, { text: inputValue(e) })}
            onchange={() => editor.commit('notes')}
            aria-invalid={error !== undefined}
            class="{INPUT_CLASS} resize-y"
          ></textarea>
          <div class="flex min-w-0 items-center gap-2">
            <label class="shrink-0 text-[0.71875rem] text-fg-muted" for="agent-note-when-{index}">Shown</label>
            <select
              id="agent-note-when-{index}"
              value={note.when}
              onchange={(e) => {
                editor.editNote(index, { when: inputValue(e) as AgentNoteWhen });
                editor.commit('notes');
              }}
              class="{SELECT_CLASS} min-w-0 flex-1 truncate"
            >
              {#each NOTE_WHENS as option (option.value)}
                <option value={option.value}>{option.label}</option>
              {/each}
            </select>
            <button type="button" class="{GHOST_BUTTON_CLASS} ml-auto" onclick={() => editor.remove('notes', index)}>
              Delete
            </button>
          </div>
          {#if entry.theirs !== undefined}
            {@render changedElsewhere(
              'agent-note-conflict',
              entry.theirs !== null,
              () => editor.keepMine('notes', index),
              () => editor.takeTheirs('notes', index),
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
          disabled={editor.notes.entries.length >= MAX_AGENT_NOTES}
          onclick={() => editor.addNote()}
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
        <button type="button" class={SECONDARY_BUTTON_CLASS} onclick={() => void editor.restore()} data-testid="agent-profiles-restore">
          Restore
        </button>
      </SettingsField>
      {#if editor.restoreRefusal}
        <p class="text-[0.71875rem] text-error" role="alert" data-testid="agent-profiles-restore-refusal">{editor.restoreRefusal}</p>
      {/if}
    </div>
  </section>
  {/if}
</div>
