<script lang="ts">
  // Settings → Voice. Two owners on one page:
  //
  // Dictation into the composer belongs to the computer the page names: a
  // thread dictates through the computer it runs on. Its two sources are
  // host-tier switches, off until turned on: the computer's Codex sign-in
  // with ChatGPT, and live transcription through an OpenAI API key the
  // user puts in the computer's owner-only key file (never shown here).
  // Live wins where both are on; with no source on, no composer shows a
  // mic.
  //
  // Read-aloud belongs to THIS DEVICE: its voices speak, and its
  // preferences (`stores/readAloudPreferences.svelte.ts`) stay in frontend
  // storage. The section says so, and renders even when the computer's
  // settings cannot be loaded (SettingsView's `hasDeviceControls`).

  import { HOST_TIER_REASON, settingsComputer } from './settingsComputer';
  import SettingsField from './SettingsField.svelte';
  import SettingsHeader from './SettingsHeader.svelte';
  import SettingsCallout from './SettingsCallout.svelte';
  import ToggleSwitch from '../shared/ToggleSwitch.svelte';
  import CopyButton from '../primitives/CopyButton.svelte';
  import { INPUT_CLASS, SECONDARY_BUTTON_CLASS, SECTION_PROSE_CLASS, SELECT_CLASS } from './styles';
  import {
    dictationLiveReady,
    dictationSignedIn,
    dictationSubscriptionReady,
    refreshDictationSources,
  } from '../../stores/dictation.svelte';
  import { EditOpenAIKeyFile, OpenAIKeyFile, type OpenAIKeyFileStatus } from '../../stores/bindings';
  import { addToast } from '../../stores/toast.svelte';
  import { chordHintSuffix } from '../../stores/keybindings.svelte';
  import { readAloud, readAloudPhase, readAloudVoices, stopReadAloud } from '../../stores/readAloud.svelte';
  import {
    READ_ALOUD_RATE,
    READ_ALOUD_VOLUME,
    readAloudPreferences,
    setReadAloudPreference,
  } from '../../stores/readAloudPreferences.svelte';
  import { onDestroy, onMount } from 'svelte';

  const { backend, call, hasScope, getSettings, updateSetting, hostTierWritable } = settingsComputer();

  let settings = $derived(getSettings());
  let granted = $derived(hasScope('threads:operate'));
  let on = $derived(settings.dictationSubscriptionEnabled);
  let signedIn = $derived(dictationSignedIn(backend));
  let ready = $derived(dictationSubscriptionReady(backend));
  // Host tier: each switch spends this computer's credential on every
  // client's dictation (internal/settings/tier.go).
  let hostWritable = $derived(hostTierWritable());
  let status = $derived(ready ? 'Ready' : !on ? 'Off' : signedIn ? 'Checking…' : 'Codex is not signed in with ChatGPT');
  let hint = $derived(on
    ? `The mic dictates at the caret and never sends${chordHintSuffix('composer.dictate')}.`
    : 'Show a mic in the composer that dictates through this computer’s Codex sign-in with ChatGPT.');

  $effect(() => {
    if (granted) void refreshDictationSources(backend);
  });

  // ---- live, through the API key ----

  const DELAYS = ['minimal', 'low', 'medium', 'high', 'xhigh'] as const;

  let liveOn = $derived(settings.dictationAPIEnabled);
  let liveReady = $derived(dictationLiveReady(backend));
  // Only a connection on the computer can open its editor; the file is
  // owner-only there, so a remote client puts the key in it itself.
  let onHost = $derived(hasScope('host'));
  let keyFile = $state<OpenAIKeyFileStatus | null>(null);
  let keyError = $state('');
  // The editor action's own failure, which no status read replaces.
  let editError = $state('');
  let editing = $state(false);
  let keyState = $derived(keyFile?.state ?? '');
  let keyProblem = $derived(
    keyState === 'missing' ? 'No key file yet.'
      : keyState === 'empty' ? 'The key file is empty.'
        : keyState === 'refused' ? (keyFile?.problem || 'The key file cannot be used.')
          : '',
  );
  let keyLine = $derived(editError || keyError || keyProblem || (keyState === 'present' ? 'Key present.' : 'Checking…'));
  let liveStatus = $derived(liveReady ? 'Ready' : !liveOn ? 'Off' : keyProblem || keyError || 'Checking…');
  let liveHint = $derived(liveOn
    ? 'Words appear as you speak. Live wins when both sources are on.'
    : 'Show a mic that streams speech to OpenAI’s transcription API with the key in this computer’s key file.');

  // Each read or edit takes the next generation; only the newest one's
  // answer or error lands, so a slow read never undoes a newer one.
  let keyGeneration = 0;

  async function readKeyFile(): Promise<void> {
    if (!hasScope('settings:read')) return;
    const generation = ++keyGeneration;
    try {
      const file = await call(() => OpenAIKeyFile());
      if (generation !== keyGeneration) return;
      keyFile = file;
      keyError = '';
    } catch (err) {
      if (generation !== keyGeneration) return;
      keyError = err instanceof Error ? err.message : 'Could not read the key file.';
    }
  }

  async function editKeyFile(): Promise<void> {
    editing = true;
    editError = '';
    try {
      await call(() => EditOpenAIKeyFile());
      // Its reply was read before the editor opened; a fresh read, ordered
      // with the others, says what is there now.
      void readKeyFile();
    } catch (err) {
      editError = err instanceof Error ? err.message : 'Could not open the key file.';
    } finally {
      editing = false;
    }
    if (granted) void refreshDictationSources(backend);
  }

  // The key is edited outside the app; coming back to the window re-reads
  // the file, as the store re-reads the sources.
  onMount(() => {
    void readKeyFile();
    const refresh = (): void => {
      if (document.visibilityState === 'visible') void readKeyFile();
    };
    window.addEventListener('focus', refresh);
    document.addEventListener('visibilitychange', refresh);
    return () => {
      window.removeEventListener('focus', refresh);
      document.removeEventListener('visibilitychange', refresh);
    };
  });

  // Lists are typed as text: languages separated by commas or spaces,
  // keywords one per line. The computer validates each entry and refuses
  // the write with the reason, which the settings store shows.
  const splitLanguages = (text: string): string[] => text.split(/[\s,]+/).map((s) => s.trim()).filter(Boolean);
  const splitLines = (text: string): string[] => text.split(/\r?\n/).map((s) => s.trim()).filter(Boolean);

  // ---- read aloud (this device) ----

  const SAMPLE_KEY = 'settings-sample';
  const SAMPLE = 'This is how replies will sound when they are read aloud.';

  let prefs = $derived(readAloudPreferences());
  let voices = $derived(readAloudVoices());
  let hasVoices = $derived(voices.length > 0);
  let controlsOn = $derived(hasVoices && prefs.enabled);
  let sampling = $derived(readAloudPhase(SAMPLE_KEY) === 'playing');
  let readAloudHint = $derived(hasVoices
    ? `Each finished reply gets a play button beside Copy${chordHintSuffix('readAloud.toggle')}.`
    : 'This device has no voices to read with.');

  function trySample(): void {
    if (sampling) stopReadAloud();
    else readAloud(SAMPLE_KEY, SAMPLE);
  }

  // Leaving the page must not leave the sample speaking.
  onDestroy(() => {
    if (readAloudPhase(SAMPLE_KEY) !== 'idle') stopReadAloud();
  });

  const percent = (value: number): string => `${Math.round(value * 100)}%`;
</script>

<div class="settings-sections">
  <section data-testid="settings-voice-section" class="flex flex-col gap-3">
    <SettingsHeader title="Dictation" description="Speak into the composer. Transcription runs on this computer." />
    <SettingsField id="voice.dictation" label="Dictate with ChatGPT" {hint}>
      <ToggleSwitch
        checked={on}
        disabled={!hostWritable}
        title={hostWritable ? undefined : HOST_TIER_REASON}
        ariaLabel="Toggle dictation with ChatGPT"
        onToggle={(value) => updateSetting('dictationSubscriptionEnabled', value)}
      />
    </SettingsField>
    {#if granted}
      <p
        data-testid="voice-dictation-status"
        data-available={ready}
        class="text-[0.75rem] {ready ? 'text-fg' : 'text-fg-muted'}"
      >
        {status}
      </p>
    {:else}
      <SettingsCallout>
        This connection does not have permission to dictate on this computer.
      </SettingsCallout>
    {/if}
    <p class={SECTION_PROSE_CLASS} data-testid="voice-dictation-disclosure">
      A recording goes from this computer to OpenAI's transcription service under that
      sign-in, and this computer keeps it in memory only until the text comes back. OpenAI
      may keep the audio for up to 30 days.
    </p>

    <SettingsField id="voice.dictation-live" label="Dictate live with an OpenAI API key" hint={liveHint}>
      <ToggleSwitch
        checked={liveOn}
        disabled={!hostWritable}
        title={hostWritable ? undefined : HOST_TIER_REASON}
        ariaLabel="Toggle live dictation with an OpenAI API key"
        onToggle={(value) => updateSetting('dictationAPIEnabled', value)}
      />
    </SettingsField>
    {#if granted}
      <p
        data-testid="voice-dictation-live-status"
        data-available={liveReady}
        class="text-[0.75rem] {liveReady ? 'text-fg' : 'text-fg-muted'}"
      >
        {liveStatus}
      </p>
    {/if}
    <div
      data-settings-field="voice.dictation-live-key"
      data-settings-label="API key file"
      data-testid="voice-dictation-live-key"
      data-key-state={keyState}
      class="flex flex-col gap-1 rounded-[var(--radius-field)] py-1.5"
    >
      <p class="text-[0.8125rem] font-medium text-fg">API key file</p>
      <p class="text-[0.71875rem] leading-snug text-fg-muted">
        {onHost
          ? 'Owner-only on this computer; the key never leaves it.'
          : 'Put the key in this file on that computer; it is owner-only there and never leaves it.'}
      </p>
      {#if keyFile}
        <div class="flex items-center gap-1">
          <code
            data-testid="voice-dictation-live-key-path"
            class="min-w-0 truncate rounded-[var(--radius-field)] bg-surface-2/50 px-1.5 py-0.5 font-mono text-[0.6875rem] text-fg"
            title={keyFile.path}
          >{keyFile.path}</code>
          <CopyButton text={keyFile.path} label="Copy key file path" size="xs" iconSize={12} onError={() => addToast('error', 'Failed to copy')} />
          {#if onHost}
            <button
              type="button"
              class="{SECONDARY_BUTTON_CLASS} ml-auto"
              disabled={editing}
              data-testid="voice-dictation-live-key-edit"
              onclick={() => void editKeyFile()}
            >
              Edit
            </button>
          {/if}
        </div>
      {/if}
      <p
        data-testid="voice-dictation-live-key-line"
        class="text-[0.75rem] {editError || keyError || keyProblem ? 'text-error' : 'text-fg-muted'}"
      >
        {keyLine}
      </p>
    </div>
    {#if liveOn}
      <SettingsField id="voice.dictation-live-delay" label="Delay" hint="How long OpenAI waits for a sentence to settle before transcribing it." htmlFor="dictation-live-delay">
        <select
          id="dictation-live-delay"
          data-testid="voice-dictation-live-delay"
          class={SELECT_CLASS}
          disabled={!hostWritable}
          title={hostWritable ? undefined : HOST_TIER_REASON}
          value={settings.dictationAPIDelay}
          onchange={(e) => updateSetting('dictationAPIDelay', (e.target as HTMLSelectElement).value)}
        >
          <option value="">OpenAI’s default</option>
          {#each DELAYS as delay (delay)}
            <option value={delay}>{delay}</option>
          {/each}
        </select>
      </SettingsField>
      <SettingsField id="voice.dictation-live-languages" label="Languages" hint="ISO 639 codes you speak, separated by commas; empty lets OpenAI detect." htmlFor="dictation-live-languages">
        <input
          id="dictation-live-languages"
          type="text"
          data-testid="voice-dictation-live-languages"
          disabled={!hostWritable}
          title={hostWritable ? undefined : HOST_TIER_REASON}
          value={settings.dictationAPILanguages.join(', ')}
          onchange={(e) => updateSetting('dictationAPILanguages', splitLanguages((e.target as HTMLInputElement).value))}
          placeholder="en, fr"
          class="{INPUT_CLASS} max-w-[16rem]"
        />
      </SettingsField>
      <SettingsField id="voice.dictation-live-keywords" label="Keywords" hint="Names and terms to spell as written, one per line." htmlFor="dictation-live-keywords" align="start">
        <textarea
          id="dictation-live-keywords"
          data-testid="voice-dictation-live-keywords"
          disabled={!hostWritable}
          title={hostWritable ? undefined : HOST_TIER_REASON}
          rows={3}
          value={settings.dictationAPIKeywords.join('\n')}
          onchange={(e) => updateSetting('dictationAPIKeywords', splitLines((e.target as HTMLTextAreaElement).value))}
          placeholder="Agent Overflow"
          class="{INPUT_CLASS} max-w-[24rem] resize-none"
        ></textarea>
      </SettingsField>
      <SettingsField id="voice.dictation-live-prompt" label="Prompt" hint="Context for the transcription, such as what you dictate about." htmlFor="dictation-live-prompt" align="start">
        <textarea
          id="dictation-live-prompt"
          data-testid="voice-dictation-live-prompt"
          disabled={!hostWritable}
          title={hostWritable ? undefined : HOST_TIER_REASON}
          rows={3}
          value={settings.dictationAPIPrompt}
          onchange={(e) => updateSetting('dictationAPIPrompt', (e.target as HTMLTextAreaElement).value)}
          placeholder="Messages to a coding agent about a Go and Svelte app."
          class="{INPUT_CLASS} max-w-[24rem] resize-none"
        ></textarea>
      </SettingsField>
    {/if}
    <p class={SECTION_PROSE_CLASS} data-testid="voice-dictation-live-disclosure">
      Live audio goes from this computer to OpenAI's API under that key. OpenAI does not
      train on API data by default and may keep it for up to 30 days for abuse monitoring.
    </p>
  </section>

  <section data-testid="settings-read-aloud-section" data-has-voices={hasVoices} class="flex flex-col gap-1">
    <SettingsHeader
      title="Read aloud"
      description="Hear replies in this device’s own voice. Saved on this device, not the computer."
    />
    <SettingsField id="voice.read-aloud" label="Read replies aloud" hint={readAloudHint}>
      <ToggleSwitch
        checked={prefs.enabled}
        disabled={!hasVoices}
        title={hasVoices ? undefined : 'This device has no voices to read with.'}
        ariaLabel="Toggle reading replies aloud"
        onToggle={(value) => setReadAloudPreference('enabled', value)}
      />
    </SettingsField>
    <SettingsField id="voice.read-aloud-voice" label="Voice" hint="One of this device's voices." htmlFor="read-aloud-voice">
      <select
        id="read-aloud-voice"
        data-testid="read-aloud-voice-select"
        class={SELECT_CLASS}
        disabled={!controlsOn}
        value={prefs.voiceId}
        onchange={(e) => setReadAloudPreference('voiceId', (e.target as HTMLSelectElement).value)}
      >
        <option value="">System default</option>
        {#each voices as voice (voice.id)}
          <option value={voice.id}>{voice.name} ({voice.lang})</option>
        {/each}
      </select>
    </SettingsField>
    <SettingsField id="voice.read-aloud-rate" label="Speed" htmlFor="read-aloud-rate" stacked>
      <div class="grid grid-cols-[minmax(0,1fr)_auto] items-center gap-3">
        <input
          id="read-aloud-rate"
          type="range"
          min={READ_ALOUD_RATE.min}
          max={READ_ALOUD_RATE.max}
          step={READ_ALOUD_RATE.step}
          value={prefs.rate}
          disabled={!controlsOn}
          oninput={(e) => setReadAloudPreference('rate', Number((e.target as HTMLInputElement).value))}
          aria-label="Reading speed"
          data-testid="read-aloud-rate-slider"
          class="w-full accent-accent disabled:opacity-50"
        />
        <span class="w-10 text-right text-[0.71875rem] tabular-nums text-fg-muted" data-testid="read-aloud-rate-value">
          {prefs.rate.toFixed(1)}×
        </span>
      </div>
    </SettingsField>
    <SettingsField id="voice.read-aloud-volume" label="Volume" htmlFor="read-aloud-volume" stacked>
      <div class="grid grid-cols-[minmax(0,1fr)_auto] items-center gap-3">
        <input
          id="read-aloud-volume"
          type="range"
          min={READ_ALOUD_VOLUME.min}
          max={READ_ALOUD_VOLUME.max}
          step={READ_ALOUD_VOLUME.step}
          value={prefs.volume}
          disabled={!controlsOn}
          oninput={(e) => setReadAloudPreference('volume', Number((e.target as HTMLInputElement).value))}
          aria-label="Reading volume"
          data-testid="read-aloud-volume-slider"
          class="w-full accent-accent disabled:opacity-50"
        />
        <span class="w-10 text-right text-[0.71875rem] tabular-nums text-fg-muted" data-testid="read-aloud-volume-value">
          {percent(prefs.volume)}
        </span>
      </div>
    </SettingsField>
    <div class="pt-1">
      <button
        type="button"
        class={SECONDARY_BUTTON_CLASS}
        disabled={!controlsOn}
        aria-pressed={sampling}
        data-testid="read-aloud-try"
        onclick={trySample}
      >
        {sampling ? 'Stop' : 'Try it'}
      </button>
    </div>
  </section>
</div>
