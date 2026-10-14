<script lang="ts">
  // The System prompt section of a provider's settings page: the ordered
  // list of system-prompt overrides, each a file on the computer, plus the
  // placeholder reference and a copy of the instructions an agent needs to
  // edit the files. The page owns the heading and the line about when a
  // change takes effect; the tool list and Claude's session axes are
  // sections of their own beside this one.
  //
  // This component owns every write for the block: the enable switch, the
  // model chips, add and remove all commit immediately against the settings
  // store. The prompt text itself is never written from here; it lives in
  // the file, which the Edit control opens.

  import ClipboardCopy from '@lucide/svelte/icons/clipboard-copy';
  import Icon from '../primitives/Icon.svelte';
  import { settingsComputer } from './settingsComputer';
  const { getSettings, updateSettingsPatch, backend, hasScope } = settingsComputer();
  import { openInEditor } from '../../stores/openInEditor';
  import { addErrorToast, addToast } from '../../stores/toast.svelte';
  import { errString } from '../../utils/errors';
  import { copyToClipboard } from '../../utils/clipboard';
  import {
    ensureProviderModels,
    getProviderModels,
  } from '../../stores/providerModels.svelte';
  import type { ProviderDefinition } from '../../providers/catalog';
  import type { PromptOverride } from '../../types/settings';
  import {
    agentPromptInstructions,
    promptOverridesFor,
    promptOverridesSettingsKey,
    shadowedModels,
    withEntryAdded,
    withEntryPatch,
    withEntryRemoved,
  } from '../../utils/promptOverrides';
  import PromptOverrideEntry from './PromptOverrideEntry.svelte';
  import PromptPlaceholderLegend from './PromptPlaceholderLegend.svelte';
  import SettingsField from './SettingsField.svelte';
  import type { ProviderFieldId } from './fields';
  import { SECONDARY_BUTTON_CLASS } from './styles';

  let { provider }: { provider: ProviderDefinition } = $props();

  // ProviderDefinition.id spans every provider; the field index only covers
  // the two that have a page, and this section only ever renders on one of
  // them. The ternary narrows without a cast.
  let fieldId = $derived<ProviderFieldId>(
    provider.id === 'codex' ? 'codex.system-prompt' : 'claude.system-prompt',
  );

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
  let models = $derived(getProviderModels(provider.id, backend));
  let catalogError = $state<string | null>(null);
  // The instructions name the files, so there is nothing to copy until an
  // entry has been saved and has one.
  let savedPaths = $derived(entries.flatMap((entry) => (entry.path ? [entry.path] : [])));

  // The catalog is a cached store read; ensure() is a no-op once loaded, so
  // opening this tab after Providers costs nothing. A failure is rendered as
  // state next to the chips instead of a toast: the section is still usable,
  // the model list just cannot be offered.
  $effect(() => {
    const id = provider.id;
    let cancelled = false;
    ensureProviderModels(id, backend)
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

  async function writeEntries(next: PromptOverride[]): Promise<void> {
    const key = promptOverridesSettingsKey(provider.id);
    if (!key) return;
    await updateSettingsPatch({ [key]: next });
  }

  function patchEntry(index: number, patch: Partial<PromptOverride>): void {
    void writeEntries(withEntryPatch(entries, index, patch));
  }

  function removeEntry(index: number): void {
    void writeEntries(withEntryRemoved(entries, index));
  }

  function addEntry(): void {
    void writeEntries(withEntryAdded(entries));
  }

  // The prompt file lives on the computer these settings belong to, and the
  // editor opens on that computer's desktop, so the control is offered only
  // where the page can act there. The path and the copied instructions are
  // shown everywhere: an agent on that computer can still be pointed at them.
  let canOpenFiles = $derived(hasScope('host'));

  async function openPromptFile(path: string): Promise<void> {
    try {
      await openInEditor(backend, path, 0, 0, '', '');
    } catch (err) {
      addErrorToast(errString(err), err);
    }
  }

  async function copyInstructions(event: MouseEvent): Promise<void> {
    if (await copyToClipboard(agentPromptInstructions(savedPaths), event)) {
      addToast('success', 'Copied. Paste it into an agent to have it edit the prompt files.');
    } else {
      addToast('error', 'Could not copy to the clipboard.');
    }
  }
</script>

<div data-testid="settings-prompts-{provider.id}">
  <SettingsField
    id={fieldId}
    label="System prompt overrides"
    hint="Each override is a Markdown file in the computer's prompts folder; its text replaces the provider's default system prompt for the models you pick. Edit the file in your editor or hand it to an agent. The first enabled entry whose models include the session's model wins; a .md file in the folder without an entry is ignored."
    align="start"
    stacked
  >
    <div class="flex flex-col gap-3">
      {#if rows.length === 0}
        <p
          class="text-[0.71875rem] text-fg-hint"
          data-testid="settings-prompts-{provider.id}-empty"
        >
          No overrides. {provider.label} uses its own system prompt.
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
                onPatch={(patch) => patchEntry(row.index, patch)}
                onRemove={() => removeEntry(row.index)}
                onOpenFile={canOpenFiles ? (path) => void openPromptFile(path) : null}
              />
            </li>
          {/each}
        </ul>

        <PromptPlaceholderLegend provider={provider.id} />
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

      <div class="flex flex-wrap items-center gap-2">
        <button
          type="button"
          class={SECONDARY_BUTTON_CLASS}
          data-testid="settings-prompt-add-{provider.id}"
          onclick={addEntry}
        >
          Add override
        </button>
        {#if savedPaths.length > 0}
          <button
            type="button"
            class="{SECONDARY_BUTTON_CLASS} inline-flex items-center gap-1.5"
            data-testid="settings-prompt-copy-instructions-{provider.id}"
            title="Copy the file paths and the README location for an agent to edit the prompts"
            onclick={(event) => void copyInstructions(event)}
          >
            <Icon icon={ClipboardCopy} size={12} strokeWidth={2} />
            Copy instructions for an agent
          </button>
        {/if}
      </div>
    </div>
  </SettingsField>
</div>
