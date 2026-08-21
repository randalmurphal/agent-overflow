<script lang="ts">
  // One provider's block in Settings → Prompts & Tools: the ordered list of
  // system-prompt overrides, the placeholder legend that writes into them,
  // and the provider's disabled-tool editor.
  //
  // This component owns every write for the block. The entries are edited
  // in place against the settings store — there is no draft copy and no Save
  // button: a textarea commits on `change` (i.e. on blur, which fires before
  // the click of whatever button stole focus, so a structural edit can never
  // drop what was just typed), and every other control commits immediately.

  import { tick } from 'svelte';
  import { getSettings, updateSettingsPatch } from '../../stores/settings.svelte';
  import {
    ensureProviderModels,
    getProviderModels,
  } from '../../stores/providerModels.svelte';
  import { addToast } from '../../stores/toast.svelte';
  import type { ProviderDefinition } from '../../providers/catalog';
  import type { PromptOverride } from '../../types/settings';
  import {
    insertAtSelection,
    promptOverridesFor,
    promptOverridesSettingsKey,
    shadowedModels,
    withEntryAdded,
    withEntryPatch,
    withEntryRemoved,
  } from '../../utils/promptOverrides';
  import ClaudeDisabledToolsEditor from './ClaudeDisabledToolsEditor.svelte';
  import ClaudeCrossSessionEditor from './ClaudeCrossSessionEditor.svelte';
  import ClaudeSessionAxesEditor from './ClaudeSessionAxesEditor.svelte';
  import CodexDisabledToolsEditor from './CodexDisabledToolsEditor.svelte';
  import PromptOverrideEntry from './PromptOverrideEntry.svelte';
  import PromptPlaceholderLegend from './PromptPlaceholderLegend.svelte';
  import SettingsField from './SettingsField.svelte';
  import SettingsHeader from './SettingsHeader.svelte';
  import { SECONDARY_BUTTON_CLASS } from './styles';

  let { provider }: { provider: ProviderDefinition } = $props();

  let settings = $derived(getSettings());
  let entries = $derived(promptOverridesFor(settings, provider.id));
  // One walk for the whole list: the per-entry "an earlier entry already
  // claims these models" answer needs the list, so the rows carry it rather
  // than each entry re-deriving it.
  let rows = $derived(
    entries.map((entry, index) => ({
      entry,
      index,
      shadowed: shadowedModels(entries, index),
    })),
  );
  let models = $derived(getProviderModels(provider.id));
  let catalogError = $state<string | null>(null);

  // The catalog is a cached store read; ensure() is a no-op once loaded, so
  // opening this tab after Providers costs nothing. A failure is rendered as
  // state next to the chips instead of a toast — the section is still usable,
  // the model list just cannot be offered.
  $effect(() => {
    const id = provider.id;
    let cancelled = false;
    ensureProviderModels(id)
      .then(() => {
        if (!cancelled) catalogError = null;
      })
      .catch((err: unknown) => {
        if (cancelled) return;
        console.error(`Failed to load ${id} models:`, err);
        catalogError = `Could not load the ${provider.label} model catalog.`;
      });
    return () => {
      cancelled = true;
    };
  });

  // Where a legend click inserts: the prompt the user last touched, falling
  // back to the block's first prompt. Not $state — nothing renders from it.
  let blockEl = $state<HTMLElement | null>(null);
  let lastEditor: { el: HTMLTextAreaElement; index: number } | null = null;

  function noteEditorFocus(el: HTMLTextAreaElement, index: number): void {
    lastEditor = { el, index };
  }

  function insertTarget(): { el: HTMLTextAreaElement; index: number } | null {
    if (lastEditor && lastEditor.el.isConnected) return lastEditor;
    const first = blockEl?.querySelector<HTMLTextAreaElement>('[data-prompt-editor]');
    return first ? { el: first, index: 0 } : null;
  }

  async function writeEntries(next: PromptOverride[]): Promise<void> {
    const key = promptOverridesSettingsKey(provider.id);
    if (!key) return;
    await updateSettingsPatch({ [key]: next });
  }

  // Returns the write so the legend's insert can place the caret once the
  // store has settled; the row callbacks fire and forget, as every other
  // control in Settings does.
  function patchEntry(index: number, patch: Partial<PromptOverride>): Promise<void> {
    return writeEntries(withEntryPatch(entries, index, patch));
  }

  function removeEntry(index: number): void {
    if (lastEditor?.index === index) lastEditor = null;
    void writeEntries(withEntryRemoved(entries, index));
  }

  function addEntry(): void {
    void writeEntries(withEntryAdded(entries));
  }

  async function insertToken(token: string): Promise<void> {
    const target = insertTarget();
    if (!target) {
      addToast('info', 'Click into a prompt to insert a placeholder.');
      return;
    }
    const { el, index } = target;
    const { text, caret } = insertAtSelection(
      el.value,
      el.selectionStart ?? el.value.length,
      el.selectionEnd ?? el.value.length,
      token,
    );
    lastEditor = { el, index };
    // The store is the only writer of this textarea, and it costs nothing to
    // keep it that way: the optimistic settings write makes the insertion
    // visible immediately, and a rollback repaints it for free. Writing
    // `el.value` here instead would arm no change event, so a save that
    // failed would leave visible text belonging to no state — not persisted,
    // and not undoable by anything the user can reach.
    await patchEntry(index, { prompt: text });
    await tick();
    if (!el.isConnected) return;
    // On success the store re-render already put `text` in the element and
    // the caret lands after the token; on a failed save the element still
    // holds the stored prompt, and the clamp keeps the caret inside it.
    const at = Math.min(caret, el.value.length);
    el.focus();
    el.setSelectionRange(at, at);
  }
</script>

<section
  bind:this={blockEl}
  class="rounded-[var(--radius-card)] border border-border-subtle bg-surface-1/30 p-5"
  data-testid="settings-prompts-{provider.id}"
>
  <SettingsHeader title={provider.label} />

  <SettingsField
    label="System prompt overrides"
    hint="Replaces the provider's default system prompt. The first enabled entry whose models include the session's model wins."
    align="start"
    stacked
  >
    <div class="flex flex-col gap-3">
      {#if rows.length === 0}
        <p
          class="text-[0.71875rem] text-fg-hint"
          data-testid="settings-prompts-{provider.id}-empty"
        >
          No overrides — {provider.label} uses its own system prompt.
        </p>
      {:else}
        <ul class="flex flex-col gap-3">
          {#each rows as row (row.index)}
            <li>
              <PromptOverrideEntry
                provider={provider.id}
                entry={row.entry}
                index={row.index}
                shadowed={row.shadowed}
                {models}
                onPatch={(patch) => void patchEntry(row.index, patch)}
                onRemove={() => removeEntry(row.index)}
                onEditorFocus={noteEditorFocus}
              />
            </li>
          {/each}
        </ul>

        <PromptPlaceholderLegend
          provider={provider.id}
          onInsert={(token) => void insertToken(token)}
        />
      {/if}

      {#if catalogError}
        <p
          class="text-[0.71875rem] text-error"
          role="alert"
          data-testid="settings-prompts-{provider.id}-catalog-error"
        >
          {catalogError}
        </p>
      {/if}

      <div>
        <button
          type="button"
          class={SECONDARY_BUTTON_CLASS}
          data-testid="settings-prompt-add-{provider.id}"
          onclick={addEntry}
        >
          Add override
        </button>
      </div>
    </div>
  </SettingsField>

  <div class="mt-5">
    {#if provider.id === 'codex'}
      <CodexDisabledToolsEditor {provider} />
    {:else}
      <ClaudeDisabledToolsEditor {provider} />
    {/if}
  </div>

  <!--
    Headless Claude only. claude-tui launches through a PTY with no
    `--settings` flag, so these axes cannot reach that binary and must not be
    offered under its heading.
  -->
  {#if provider.id === 'claude'}
    <div class="mt-5 border-t border-border-subtle/60 pt-4">
      <ClaudeSessionAxesEditor />
    </div>
    <div class="mt-5 border-t border-border-subtle/60 pt-4">
      <ClaudeCrossSessionEditor />
    </div>
  {/if}
</section>
