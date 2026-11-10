<script lang="ts">
  // Settings → Voice. Two owners on one page:
  //
  // Dictation into the composer belongs to the computer the page names: a
  // thread dictates through the computer it runs on, and dictation through
  // that computer's Codex sign-in with ChatGPT is a host-tier switch, off
  // until turned on; with no source on, no composer shows a mic.
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
  import { SECONDARY_BUTTON_CLASS, SECTION_PROSE_CLASS, SELECT_CLASS } from './styles';
  import { dictationAvailable, dictationSignedIn, refreshDictationSources } from '../../stores/dictation.svelte';
  import { chordHintSuffix } from '../../stores/keybindings.svelte';
  import { readAloud, readAloudPhase, readAloudVoices, stopReadAloud } from '../../stores/readAloud.svelte';
  import {
    READ_ALOUD_RATE,
    READ_ALOUD_VOLUME,
    readAloudPreferences,
    setReadAloudPreference,
  } from '../../stores/readAloudPreferences.svelte';
  import { onDestroy } from 'svelte';

  const { backend, hasScope, getSettings, updateSetting, hostTierWritable } = settingsComputer();

  let settings = $derived(getSettings());
  let granted = $derived(hasScope('threads:operate'));
  let on = $derived(settings.dictationSubscriptionEnabled);
  let signedIn = $derived(dictationSignedIn(backend));
  let ready = $derived(dictationAvailable(backend));
  // Host tier: the switch spends this computer's Codex sign-in on every
  // client's dictation (internal/settings/tier.go).
  let hostWritable = $derived(hostTierWritable());
  let status = $derived(ready ? 'Ready' : !on ? 'Off' : signedIn ? 'Checking…' : 'Codex is not signed in with ChatGPT');
  let hint = $derived(on
    ? `The mic dictates at the caret and never sends${chordHintSuffix('composer.dictate')}.`
    : 'Show a mic in the composer that dictates through this computer’s Codex sign-in with ChatGPT.');

  $effect(() => {
    if (granted) void refreshDictationSources(backend);
  });

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
