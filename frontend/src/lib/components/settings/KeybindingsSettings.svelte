<script lang="ts">
  import { onMount } from 'svelte';
  import {
    findDuplicateChordRow,
    getKeybindingIssues,
    getKeybindingLoadError,
    getKeybindingRules,
    isKeybindingsLoaded,
    isUnboundChord,
    keybindingIdentity,
    loadKeybindings,
    resetKeybindingsToDefaults,
    saveKeybindings,
    withReboundRow,
    encodeChordFromEvent,
    formatChord,
    KEYBINDING_CAPTURE_ATTR,
    UNBOUND_CHORD,
    type KeybindingRule,
  } from '../../stores/keybindings.svelte';
  import { addToast } from '../../stores/toast.svelte';
  import { errString } from '../../utils/errors';
  import SettingsCallout from './SettingsCallout.svelte';
  import SettingsHeader from './SettingsHeader.svelte';
  import { GHOST_BUTTON_CLASS, SECONDARY_BUTTON_CLASS } from './styles';

  let loaded = $derived(isKeybindingsLoaded());
  // The full effective list, not the compiled one: a row that is unbound or
  // whose chord fails to parse has no compiled form, and hiding it here
  // would make it uneditable — including un-clearable back to its default.
  let rules = $derived(getKeybindingRules());
  let issues = $derived(getKeybindingIssues());
  let loadError = $derived(getKeybindingLoadError());

  let capturingFor = $state<number | null>(null);
  let saving = $state(false);

  // Stamped on the capture control so dispatch can recognise it. Built
  // once so the spread has a stable identity across renders.
  const captureMarker = { [KEYBINDING_CAPTURE_ATTR]: '' };

  type ChordState = 'default' | 'custom' | 'unbound';

  function chordState(rule: KeybindingRule): ChordState {
    if (isUnboundChord(rule.key)) return 'unbound';
    return rule.defaultKey && rule.key !== rule.defaultKey ? 'custom' : 'default';
  }

  /** Whether this row has a shipped chord to go back to. */
  function canRestore(rule: KeybindingRule): boolean {
    return Boolean(rule.defaultKey) && rule.key !== rule.defaultKey;
  }

  /** Stable per-row test hook. Mirrors the identity tiebreak keybindingIdentity
   *  uses, minus its NUL separators, so two rows of one command stay distinct. */
  function rowTestId(rule: KeybindingRule): string {
    return rule.defaultId ?? `${rule.command}:${rule.defaultKey ?? rule.key}`;
  }

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
    const rule = rules[capturingFor];
    if (!rule) return;
    void applyChord(rule, chord);
  }

  /**
   * The one write path for every row edit. `newKey` is a captured chord, the
   * row's `defaultKey` (restore), or UNBOUND_CHORD (clear) — they differ only
   * in the confirmation toast, so the collision check, the identity-preserving
   * list rebuild, and the save can't drift between them.
   */
  async function applyChord(rule: KeybindingRule, newKey: string): Promise<void> {
    if (saving) return;
    saving = true;
    try {
      // A command can ship more than one default row (thread.new is bound to
      // both mod+n and mod+shift+o). The override model keeps one slot per
      // default identity, so it cannot express "row A took row B's chord" —
      // saving it would leave two rows of the same command showing the same
      // chord, only the first of which dispatch can ever reach. Refuse, and
      // say which row is in the way. Clearing is exempt (an unbound row is
      // unreachable, so two of them collide with nothing) — that exemption
      // lives in findDuplicateChordRow, not here.
      const duplicate = findDuplicateChordRow(rules, rule, newKey);
      if (duplicate) {
        addToast(
          'error',
          `${formatChord(newKey)} is already bound to ${duplicate.command} in this context — change that shortcut first`,
        );
        return;
      }
      // Preserve the full effective list while replacing only the selected
      // default identity. The backend re-merges by defaultId on read, and
      // saveKeybindings drops the rows that carry no override.
      await saveKeybindings(withReboundRow(rules, rule, newKey));
      if (isUnboundChord(newKey)) {
        addToast('info', `Cleared the shortcut for ${rule.command}`);
      } else {
        // Display spelling, matching the table and the collision message above —
        // the raw `mod+…` encoding is not what the row shows.
        addToast('success', `Rebound ${rule.command} to ${formatChord(newKey)}`);
      }
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
    title="Keybindings"
    description="Customize keyboard shortcuts. Click a chord to re-bind; press the desired key combination or Escape to cancel. Shortcuts use mod as ⌘ on macOS and Ctrl elsewhere."
  />

  {#if loadError}
    <SettingsCallout tone="error">
      <p class="font-semibold">Your keybindings file could not be read</p>
      <p class="mt-1.5 font-mono text-[0.71875rem]">{loadError}</p>
      <p class="mt-1.5">
        The shipped defaults are shown below. Changing any shortcut here replaces
        the file, discarding whatever it still holds — copy it somewhere safe first
        if you want to keep it.
      </p>
    </SettingsCallout>
  {/if}

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
          <th class="px-3 py-2 text-right font-medium"><span class="sr-only">Actions</span></th>
        </tr>
      </thead>
      <tbody>
        {#each rules as rule, i (keybindingIdentity(rule))}
          {@const state = chordState(rule)}
          <tr class="border-t border-border-subtle/60" data-testid="keybinding-row-{rowTestId(rule)}">
            <td class="px-3 py-2 font-mono text-[0.71875rem] text-fg">
              {rule.command}
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
                  data-chord-state={state}
                  title={state === 'custom'
                    ? `Custom — default is ${formatChord(rule.defaultKey ?? '')}`
                    : state === 'unbound'
                      ? 'No shortcut'
                      : undefined}
                  class="rounded-[var(--radius-field)] px-2.5 py-1 text-[0.71875rem] font-mono cursor-pointer disabled:cursor-not-allowed transition-colors focus:outline-none focus-visible:ring-2 focus-visible:ring-accent/40
                    {state === 'unbound'
                    ? 'border border-dashed border-border-subtle bg-transparent italic text-fg-hint hover:border-accent/40 hover:text-accent'
                    : state === 'custom'
                      ? 'border border-accent/50 bg-accent/10 text-accent hover:border-accent'
                      : 'border border-border-subtle bg-surface-0 text-fg hover:border-accent/40 hover:text-accent'}"
                >
                  {state === 'unbound' ? 'Unbound' : formatChord(rule.key)}
                </button>
              {/if}
            </td>
            <td class="px-3 py-2 font-mono text-[0.6875rem] text-fg-muted">
              {rule.when ?? ''}
            </td>
            <td class="px-3 py-2 text-right whitespace-nowrap">
              {#if state !== 'unbound'}
                <button
                  type="button"
                  onclick={() => void applyChord(rule, UNBOUND_CHORD)}
                  disabled={saving}
                  aria-label="Clear the {formatChord(rule.key)} shortcut for {rule.command}"
                  class={GHOST_BUTTON_CLASS}
                >
                  Clear
                </button>
              {/if}
              {#if canRestore(rule)}
                <button
                  type="button"
                  onclick={() => void applyChord(rule, rule.defaultKey ?? '')}
                  disabled={saving}
                  aria-label="Restore {rule.command} to its default shortcut {formatChord(
                    rule.defaultKey ?? '',
                  )}"
                  class={GHOST_BUTTON_CLASS}
                >
                  Restore
                </button>
              {/if}
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
