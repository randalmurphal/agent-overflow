<script lang="ts">
  // Claude Code's machine-wide peer inbox: whether other Claude sessions on
  // this machine can find these threads and send them messages.
  //
  // Its own section rather than a row in ClaudeSessionAxesEditor for two
  // reasons. It is the only axis here that changes who can start a turn in
  // the user's thread, which deserves the space to say so; and unlike the
  // spawn-only axes beside it, saving it makes the backend queue a restart
  // for sessions already running, so the "applies to new sessions" line at
  // the top of that section would be wrong for it.
  //
  // Headless `claude` only — claude-tui is excluded on the backend, and the
  // caller renders this under the Claude heading alone.

  import { getSettings, updateSettingsPatch } from '../../stores/settings.svelte';
  import {
    CLAUDE_CROSS_SESSION_INBOUND_OPTIONS,
    crossSessionPatch,
  } from '../../utils/claudeCrossSession';
  import { createDeferredRestartNotice } from '../../utils/deferredRestartNotice.svelte';
  import type { ClaudeCrossSessionInbound } from '../../types/settings';
  import SettingsField from './SettingsField.svelte';
  import ToggleSwitch from '../shared/ToggleSwitch.svelte';
  import { SELECT_CLASS } from './styles';

  let settings = $derived(getSettings());
  let cross = $derived(settings.claudeCrossSession ?? {});
  let enabled = $derived(cross.enabled === true);
  // The RAW stored policy, not the resolved one. effectiveInbound answers the
  // RUNTIME question ("what happens to a message right now"), which is
  // deliberately '' while the feature is off — so binding the editor to it
  // showed 'accept' over a stored 'refuse', and the next save wrote that
  // fallback back, discarding the very choice crossSessionPatch goes out of
  // its way to retain across an off/on cycle. The fallback is only for a
  // setting that has never carried a policy.
  let inbound = $derived<ClaudeCrossSessionInbound>(cross.inbound || 'accept');

  let inboundHint = $derived(
    CLAUDE_CROSS_SESSION_INBOUND_OPTIONS.find((o) => o.value === inbound)?.description ?? '',
  );

  // Both halves, because both defer: the signature is what the notice is
  // about, and it moves when a save is rolled back or when the axis changes
  // again anywhere. See deferredRestartNotice.svelte.ts.
  const restartNotice = createDeferredRestartNotice(() => `${enabled}:${inbound}`);

  function save(nextEnabled: boolean, nextInbound: ClaudeCrossSessionInbound | ''): void {
    // Every axis here binds during the CLI's own setup and nothing rebinds
    // it, so any real change is a restart — including a policy change, which
    // rides the spawn-time `--settings` block.
    const changed = nextEnabled !== enabled || nextInbound !== inbound;
    void updateSettingsPatch({ claudeCrossSession: crossSessionPatch(nextEnabled, nextInbound) });
    // Armed AFTER the optimistic write, so it captures the value that was
    // saved. A save that changed nothing arms nothing and clears nothing: an
    // earlier change is still waiting on the same restart.
    if (changed) restartNotice.arm();
  }
</script>

<div class="flex flex-col gap-1" data-testid="settings-claude-cross-session">
  <SettingsField
    id="claude.cross-session"
    label="Let Claude sessions message each other"
    hint="Claude Code can list the other Claude sessions running on this machine and send them messages. Turning this on lists these threads too, so any program running as you on this machine can start a turn in an idle thread."
    align="start"
  >
    <ToggleSwitch
      checked={enabled}
      ariaLabel="Toggle cross-session messaging"
      onToggle={(value) => save(value, inbound)}
    />
  </SettingsField>

  {#if enabled}
    <SettingsField
      id="claude.cross-session-inbound"
      label="Incoming messages"
      hint="What happens when another session sends one of these threads a message."
      htmlFor="claude-cross-session-inbound"
    >
      <select
        id="claude-cross-session-inbound"
        data-testid="settings-claude-cross-session-inbound"
        class={SELECT_CLASS}
        value={inbound}
        onchange={(e) =>
          save(true, (e.currentTarget as HTMLSelectElement).value as ClaudeCrossSessionInbound)}
      >
        {#each CLAUDE_CROSS_SESSION_INBOUND_OPTIONS as option (option.value)}
          <option value={option.value}>{option.label}</option>
        {/each}
      </select>
    </SettingsField>
    <p
      class="-mt-1 text-[0.6875rem] leading-snug text-fg-hint"
      data-testid="settings-claude-cross-session-inbound-hint"
    >
      {inboundHint}
    </p>
  {/if}

  {#if restartNotice.visible}
    <p
      class="text-[0.6875rem] leading-snug text-fg-hint"
      data-testid="settings-claude-cross-session-deferred"
    >
      Sessions already running keep their current setting until they next restart.
    </p>
  {/if}
</div>
