<script lang="ts">
  import { onMount } from 'svelte';
  import {
    getKeybindingIssues,
    getKeybindingRules,
    getResolvedKeybindings,
    isKeybindingsLoaded,
    loadKeybindings,
    resetKeybindingsToDefaults,
    saveKeybindings,
    encodeChordFromEvent,
    formatChord,
    type KeybindingRule,
  } from '../../stores/keybindings.svelte';
  import { addToast } from '../../stores/toast.svelte';
  import { errString } from '../../utils/errors';
  import Button from '../primitives/Button.svelte';

  let loaded = $derived(isKeybindingsLoaded());
  let rules = $derived(getResolvedKeybindings());
  let userRules = $derived(getKeybindingRules());
  let issues = $derived(getKeybindingIssues());

  let capturingFor = $state<number | null>(null);
  let saving = $state(false);

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

  function keybindingIdentity(rule: KeybindingRule): string {
    const identityKey = rule.defaultId ?? rule.defaultKey ?? rule.key;
    return [rule.command, rule.when ?? '', identityKey].join('\0');
  }

  async function rebind(index: number, newKey: string): Promise<void> {
    if (saving) return;
    saving = true;
    try {
      const rule = rules[index];
      if (!rule) return;
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
      addToast('success', `Rebound ${rule.rule.command} to ${newKey}`);
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

<div class="space-y-6">
  <div>
    <h3 class="text-base font-semibold text-text-primary">Keybindings</h3>
    <p class="mt-1 text-sm text-text-secondary/80">
      Customize keyboard shortcuts. Click a chord to re-bind; press the desired key combination
      or Escape to cancel. Shortcuts use <code class="font-mono">mod</code> as <kbd>⌘</kbd> on macOS and <kbd>Ctrl</kbd> elsewhere.
    </p>
  </div>

  {#if issues.length > 0}
    <div class="rounded-lg border border-error/60 bg-error/10 p-3 text-xs text-error" role="alert">
      <p class="font-semibold">Configuration issues</p>
      <ul class="mt-2 space-y-1 list-disc pl-5">
        {#each issues as issue (issue.index)}
          <li>
            <span class="font-mono">{issue.rule.command}</span> — {issue.reason}
          </li>
        {/each}
      </ul>
    </div>
  {/if}

  <div class="rounded-2xl border border-border/70 bg-surface-1/70 overflow-hidden">
    <table class="w-full text-sm">
      <thead class="bg-surface-0/60 text-text-secondary/80 text-[11px] uppercase tracking-wider">
        <tr>
          <th class="text-left px-4 py-2 font-semibold">Command</th>
          <th class="text-left px-4 py-2 font-semibold">Chord</th>
          <th class="text-left px-4 py-2 font-semibold">When</th>
        </tr>
      </thead>
      <tbody>
        {#each rules as rule, i (keybindingIdentity(rule.rule))}
          <tr class="border-t border-border/50">
            <td class="px-4 py-2 font-mono text-xs text-text-primary">{rule.rule.command}</td>
            <td class="px-4 py-2">
              {#if capturingFor === i}
                <!-- svelte-ignore a11y_autofocus -->
                <button
                  type="button"
                  autofocus
                  onkeydown={handleCaptureKeydown}
                  onblur={() => { if (capturingFor === i) capturingFor = null; }}
                  class="rounded border border-accent bg-accent/15 px-3 py-1 text-xs font-mono text-accent"
                >
                  Press keys... (Esc to cancel)
                </button>
              {:else}
                <button
                  type="button"
                  onclick={() => { capturingFor = i; }}
                  disabled={saving}
                  class="rounded border border-border/60 bg-surface-0/60 px-3 py-1 text-xs font-mono text-text-primary hover:border-accent hover:text-accent cursor-pointer disabled:cursor-not-allowed"
                >
                  {formatChord(rule.rule.key)}
                </button>
              {/if}
            </td>
            <td class="px-4 py-2 text-xs text-text-secondary font-mono">{rule.rule.when ?? ''}</td>
          </tr>
        {/each}
      </tbody>
    </table>
  </div>

  <div class="flex gap-2">
    <Button variant="secondary" size="sm" onclick={handleReset} disabled={saving}>
      {#snippet children()}Reset all to defaults{/snippet}
    </Button>
  </div>

</div>
