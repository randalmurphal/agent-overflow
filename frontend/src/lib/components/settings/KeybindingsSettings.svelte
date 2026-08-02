<script lang="ts">
  import { onMount } from 'svelte';
  import {
    findDuplicateChordRow,
    getKeybindingIssues,
    getKeybindingRules,
    getResolvedKeybindings,
    isKeybindingsLoaded,
    keybindingIdentity,
    loadKeybindings,
    resetKeybindingsToDefaults,
    saveKeybindings,
    encodeChordFromEvent,
    formatChord,
    KEYBINDING_CAPTURE_ATTR,
    type KeybindingRule,
  } from '../../stores/keybindings.svelte';
  import { addToast } from '../../stores/toast.svelte';
  import { errString } from '../../utils/errors';
  import SettingsCallout from './SettingsCallout.svelte';
  import SettingsHeader from './SettingsHeader.svelte';
  import { SECONDARY_BUTTON_CLASS } from './styles';

  let loaded = $derived(isKeybindingsLoaded());
  let rules = $derived(getResolvedKeybindings());
  let userRules = $derived(getKeybindingRules());
  let issues = $derived(getKeybindingIssues());

  let capturingFor = $state<number | null>(null);
  let saving = $state(false);

  // Stamped on the capture control so dispatch can recognise it. Built
  // once so the spread has a stable identity across renders.
  const captureMarker = { [KEYBINDING_CAPTURE_ATTR]: '' };

  onMount(() => {
    if (!loaded) void loadKeybindings();
  });

  function handleCaptureKeydown(ev: KeyboardEvent): void {
    if (capturingFor === null) return;
    ev.preventDefault();
    ev.stopPropagation();
    if (ev.key === 'Escape') {
      capturingFor = null;
      return;
    }
    const chord = encodeChordFromEvent(ev);
    if (!chord) return;
    rebind(capturingFor, chord);
  }

  async function rebind(index: number, newKey: string): Promise<void> {
    if (saving) return;
    saving = true;
    try {
      const rule = rules[index];
      if (!rule) return;
      // A command can ship more than one default row (thread.new is bound to
      // both mod+n and mod+shift+o). The override model keeps one slot per
      // default identity, so it cannot express "row A took row B's chord" —
      // saving it would leave two rows of the same command showing the same
      // chord, only the first of which dispatch can ever reach. Refuse, and
      // say which row is in the way. (Clearing the other row instead would
      // need an "unbound" override value; internal/keybindings.Update rejects
      // an empty key, so there is no such value to write today.)
      const duplicate = findDuplicateChordRow(userRules, rule.rule, newKey);
      if (duplicate) {
        addToast(
          'error',
          `${formatChord(newKey)} is already bound to ${duplicate.command} in this context — change that shortcut first`,
        );
        return;
      }
      const selectedIdentity = keybindingIdentity(rule.rule);
      // Preserve the full effective list while replacing only the selected
      // default identity. The backend re-merges by defaultId on read.
      const next: KeybindingRule[] = [];
      let replaced = false;
      for (const u of userRules) {
        if (keybindingIdentity(u) === selectedIdentity) {
          next.push({ ...u, key: newKey });
          replaced = true;
          continue;
        }
        next.push(u);
      }
      if (!replaced) {
        next.push({
          key: newKey,
          command: rule.rule.command,
          when: rule.rule.when,
          defaultId: rule.rule.defaultId,
          defaultKey: rule.rule.defaultKey,
        });
      }
      await saveKeybindings(next);
      // Display spelling, matching the table and the collision message above —
      // the raw `mod+…` encoding is not what the row shows.
      addToast('success', `Rebound ${rule.rule.command} to ${formatChord(newKey)}`);
    } catch (err) {
      addToast('error', `Failed to save keybinding: ${errString(err)}`);
    } finally {
      capturingFor = null;
      saving = false;
    }
  }

  async function handleReset(): Promise<void> {
    if (saving) return;
    saving = true;
    try {
      await resetKeybindingsToDefaults();
      addToast('info', 'Keybindings reset to defaults');
    } catch (err) {
      addToast('error', `Reset failed: ${errString(err)}`);
    } finally {
      saving = false;
    }
  }
</script>

<div class="flex flex-col gap-6">
  <SettingsHeader
    eyebrow="Shortcuts"
    title="Keybindings"
    description="Customize keyboard shortcuts. Click a chord to re-bind; press the desired key combination or Escape to cancel. Shortcuts use mod as ⌘ on macOS and Ctrl elsewhere."
  />

  {#if issues.length > 0}
    <SettingsCallout tone="error">
      <p class="font-semibold">Configuration issues</p>
      <ul class="mt-1.5 ml-4 list-disc space-y-0.5">
        {#each issues as issue (issue.index)}
          <li>
            <span class="font-mono text-[0.71875rem]">{issue.rule.command}</span>
            — {issue.reason}
          </li>
        {/each}
      </ul>
    </SettingsCallout>
  {/if}

  <div
    class="overflow-hidden rounded-[var(--radius-control)] border border-border-subtle"
  >
    <table class="w-full text-[0.78125rem]">
      <thead
        class="bg-surface-1/40 text-[0.65625rem] uppercase tracking-[0.14em] text-fg-hint"
      >
        <tr>
          <th class="px-3 py-2 text-left font-medium">Command</th>
          <th class="px-3 py-2 text-left font-medium">Chord</th>
          <th class="px-3 py-2 text-left font-medium">When</th>
        </tr>
      </thead>
      <tbody>
        {#each rules as rule, i (keybindingIdentity(rule.rule))}
          <tr class="border-t border-border-subtle/60">
            <td class="px-3 py-2 font-mono text-[0.71875rem] text-fg">
              {rule.rule.command}
            </td>
            <td class="px-3 py-2">
              {#if capturingFor === i}
                <!-- svelte-ignore a11y_autofocus -->
                <!--
                  The capture marker is what stops the chord being
                  recorded from ALSO running its command (dispatchKey's
                  isKeybindingCaptureTarget guard). stopPropagation in
                  the handler below is defence in depth for the other
                  window-level listeners, not the guard.
                -->
                <button
                  type="button"
                  autofocus
                  {...captureMarker}
                  onkeydown={handleCaptureKeydown}
                  onblur={() => {
                    if (capturingFor === i) capturingFor = null;
                  }}
                  class="rounded-[var(--radius-field)] border border-accent bg-accent/15 px-2.5 py-1 text-[0.71875rem] font-mono text-accent"
                >
                  Press keys... (Esc to cancel)
                </button>
              {:else}
                <button
                  type="button"
                  onclick={() => {
                    capturingFor = i;
                  }}
                  disabled={saving}
                  class="rounded-[var(--radius-field)] border border-border-subtle bg-surface-0 px-2.5 py-1 text-[0.71875rem] font-mono text-fg hover:border-accent/40 hover:text-accent cursor-pointer disabled:cursor-not-allowed transition-colors focus:outline-none focus-visible:ring-2 focus-visible:ring-accent/40"
                >
                  {formatChord(rule.rule.key)}
                </button>
              {/if}
            </td>
            <td class="px-3 py-2 font-mono text-[0.6875rem] text-fg-muted">
              {rule.rule.when ?? ''}
            </td>
          </tr>
        {/each}
      </tbody>
    </table>
  </div>

  <div>
    <button
      type="button"
      onclick={handleReset}
      disabled={saving}
      class={SECONDARY_BUTTON_CLASS}
    >
      Reset all to defaults
    </button>
  </div>
</div>
