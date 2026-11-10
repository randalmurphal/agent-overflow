<script lang="ts">
  // Settings → Voice, read aloud. Two owners in one section:
  //
  // The preferences belong to THIS DEVICE (`stores/readAloudPreferences
  // .svelte.ts`, frontend storage): whether replies are read, the voice,
  // speed and volume. The device's own voices speak here.
  //
  // OpenAI voices belong to the computer the page names: its host-tier
  // switch, off until turned on, has it make each sentence's audio with the
  // key in its key file (the Dictation section's key file block, whose
  // problem arrives as `keyProblem`). A reply from a computer that does not
  // offer the saved OpenAI voice is read in this device's voice.

  import { HOST_TIER_REASON, settingsComputer } from './settingsComputer';
  import SettingsField from './SettingsField.svelte';
  import SettingsHeader from './SettingsHeader.svelte';
  import Play from '@lucide/svelte/icons/play';
  import Square from '@lucide/svelte/icons/square';
  import Icon from '../primitives/Icon.svelte';
  import ToggleSwitch from '../shared/ToggleSwitch.svelte';
  import { SECONDARY_BUTTON_CLASS, SECTION_PROSE_CLASS, SELECT_CLASS } from './styles';
  import { chordHintSuffix } from '../../stores/keybindings.svelte';
  import {
    openAIVoiceId,
    openAIVoiceOf,
    readAloud,
    readAloudKey,
    readAloudPhase,
    readAloudVoices,
    stopReadAloud,
  } from '../../stores/readAloud.svelte';
  import {
    readAloudSourceAvailable,
    readAloudSourceVoiceNames,
    readAloudSourceVoices,
    refreshReadAloudSource,
  } from '../../stores/readAloudSource.svelte';
  import {
    READ_ALOUD_RATE,
    READ_ALOUD_VOLUME,
    readAloudPreferences,
    setReadAloudPreference,
  } from '../../stores/readAloudPreferences.svelte';
  import { onDestroy } from 'svelte';

  let { keyProblem }: { keyProblem: string } = $props();

  const { backend, hasScope, getSettings, updateSetting, hostTierWritable } = settingsComputer();

  const SAMPLE_KEY = 'settings-sample';
  const PREVIEW_KEY = 'settings-preview:';
  const SAMPLE = 'This is how replies will sound when they are read aloud.';

  let settings = $derived(getSettings());
  let granted = $derived(hasScope('threads:operate'));
  let hostWritable = $derived(hostTierWritable());

  // The store asks nothing without permission and re-asks when it is granted.
  $effect(() => {
    void refreshReadAloudSource(backend);
  });

  // ---- OpenAI voices, this computer's ----

  let apiOn = $derived(settings.readAloudAPIEnabled);
  let apiReady = $derived(readAloudSourceAvailable(backend));
  let apiVoices = $derived(readAloudSourceVoices(backend));
  let apiStatus = $derived(apiReady ? 'Ready' : !apiOn ? 'Off' : keyProblem || 'Checking…');
  let apiHint = $derived(apiOn
    ? 'Replies from this computer can be read in its OpenAI voices.'
    : 'Offer OpenAI voices that this computer makes with the key in its key file.');

  // ---- this device ----

  let prefs = $derived(readAloudPreferences());
  let voices = $derived(readAloudVoices());
  let hasVoices = $derived(voices.length > 0);
  let canRead = $derived(hasVoices || apiReady);
  let controlsOn = $derived(canRead && prefs.enabled);
  let sampling = $derived(readAloudPhase(SAMPLE_KEY) !== 'idle');
  let readAloudHint = $derived(canRead
    ? `Each finished reply gets a play button beside Copy${chordHintSuffix('readAloud.toggle')}.`
    : 'This device has no voices to read with.');

  // A saved OpenAI voice this computer does not offer stays selected; the
  // note says what a reply from such a computer is read in.
  let savedOpenAI = $derived(openAIVoiceOf(prefs.voiceId));
  let savedUnoffered = $derived(savedOpenAI !== null && !apiVoices.some((voice) => voice.id === savedOpenAI));
  let savedName = $derived(
    savedOpenAI === null ? '' : readAloudSourceVoiceNames(backend).find((voice) => voice.id === savedOpenAI)?.name ?? savedOpenAI,
  );

  function trySample(): void {
    if (sampling) stopReadAloud();
    else readAloud(SAMPLE_KEY, SAMPLE, backend);
  }

  function preview(voice: string): void {
    const key = `${PREVIEW_KEY}${voice}`;
    if (readAloudPhase(key) !== 'idle') stopReadAloud();
    else readAloud(key, SAMPLE, backend, openAIVoiceId(voice));
  }

  // Leaving the page must not leave a sample or preview speaking.
  onDestroy(() => {
    const key = readAloudKey();
    if (key === SAMPLE_KEY || key?.startsWith(PREVIEW_KEY)) stopReadAloud();
  });

  const percent = (value: number): string => `${Math.round(value * 100)}%`;
</script>

<section data-testid="settings-read-aloud-section" data-has-voices={hasVoices} class="flex flex-col gap-1">
  <SettingsHeader
    title="Read aloud"
    description="Hear replies in this device’s own voices, or in OpenAI voices the computer makes. Saved on this device, not the computer, except the OpenAI switch."
  />
  <SettingsField id="voice.read-aloud" label="Read replies aloud" hint={readAloudHint}>
    <ToggleSwitch
      checked={prefs.enabled}
      disabled={!canRead}
      title={canRead ? undefined : 'This device has no voices to read with.'}
      ariaLabel="Toggle reading replies aloud"
      onToggle={(value) => setReadAloudPreference('enabled', value)}
    />
  </SettingsField>
  <SettingsField id="voice.read-aloud-api" label="Read aloud with OpenAI voices" hint={apiHint}>
    <ToggleSwitch
      checked={apiOn}
      disabled={!hostWritable}
      title={hostWritable ? undefined : HOST_TIER_REASON}
      ariaLabel="Toggle reading aloud with OpenAI voices"
      onToggle={(value) => updateSetting('readAloudAPIEnabled', value)}
    />
  </SettingsField>
  {#if granted}
    <p
      data-testid="voice-read-aloud-api-status"
      data-available={apiReady}
      class="text-[0.75rem] {apiReady ? 'text-fg' : 'text-fg-muted'}"
    >
      {apiStatus}
    </p>
  {/if}
  <p class={SECTION_PROSE_CLASS} data-testid="voice-read-aloud-api-disclosure">
    The text of each reply read in an OpenAI voice goes from this computer to OpenAI's API
    under the key in its key file and is billed to that key. OpenAI does not train on API
    data by default and may keep it for up to 30 days. The voices are AI-generated.
  </p>
  <SettingsField
    id="voice.read-aloud-voice"
    label="Voice"
    hint="This device’s voices speak here; OpenAI voices are made by the computer a reply runs on."
    htmlFor="read-aloud-voice"
  >
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
      {#if apiVoices.length > 0}
        <optgroup label="OpenAI voices on this computer" data-testid="read-aloud-openai-voices">
          {#each apiVoices as voice (voice.id)}
            <option value={openAIVoiceId(voice.id)}>{voice.name}</option>
          {/each}
        </optgroup>
      {/if}
      {#if savedUnoffered}
        <option value={prefs.voiceId}>{savedName} (OpenAI)</option>
      {/if}
    </select>
  </SettingsField>
  {#if savedUnoffered}
    <p data-testid="read-aloud-voice-unoffered" class="text-[0.71875rem] leading-snug text-fg-muted">
      This computer does not offer {savedName}; replies from a computer without it are read in this
      device’s voice.
    </p>
  {/if}
  {#if apiVoices.length > 0}
    <div class="flex flex-col gap-1 py-1">
      <p class="text-[0.71875rem] leading-snug text-fg-muted">
        Preview an OpenAI voice. The saved voice does not change.
      </p>
      <ul data-testid="read-aloud-openai-previews" class="flex flex-wrap gap-1.5">
        {#each apiVoices as voice (voice.id)}
          {@const previewing = readAloudPhase(`${PREVIEW_KEY}${voice.id}`) !== 'idle'}
          <li>
            <button
              type="button"
              class="{SECONDARY_BUTTON_CLASS} inline-flex items-center gap-1.5"
              disabled={!controlsOn}
              aria-pressed={previewing}
              aria-label={previewing ? `Stop the ${voice.name} preview` : `Preview ${voice.name}`}
              data-testid="read-aloud-openai-preview"
              data-voice={voice.id}
              onclick={() => preview(voice.id)}
            >
              <Icon icon={previewing ? Square : Play} size={11} />
              {voice.name}
            </button>
          </li>
        {/each}
      </ul>
    </div>
  {/if}
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
