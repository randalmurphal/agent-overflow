<script lang="ts">
  // Settings → Voice: dictation into the composer. A thread dictates through
  // the computer it runs on. Dictation through that computer's Codex
  // sign-in with ChatGPT is a host-tier switch, off until turned on; with no
  // source on, no composer shows a mic.

  import { HOST_TIER_REASON, settingsComputer } from './settingsComputer';
  import SettingsField from './SettingsField.svelte';
  import SettingsCallout from './SettingsCallout.svelte';
  import ToggleSwitch from '../shared/ToggleSwitch.svelte';
  import { SECTION_PROSE_CLASS } from './styles';
  import { dictationAvailable, dictationSignedIn, refreshDictationSources } from '../../stores/dictation.svelte';
  import { chordHintSuffix } from '../../stores/keybindings.svelte';

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
</script>

<section data-testid="settings-voice-section" class="flex flex-col gap-3">
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
