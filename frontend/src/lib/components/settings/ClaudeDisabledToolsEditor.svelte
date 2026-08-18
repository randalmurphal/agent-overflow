<script lang="ts">
  // Claude's disabled-tool list: free-form names, because Claude takes them
  // on `--disallowedTools` verbatim and its tool set changes release to
  // release. The suggestion row is a convenience over the common built-ins,
  // never a closed set — a name AO has never heard of is a legitimate entry,
  // and one the CLI has never heard of is simply ignored.

  import { getSettings, updateSettingsPatch } from '../../stores/settings.svelte';
  import {
    CLAUDE_TOOL_SUGGESTIONS,
    disabledToolNameError,
    disabledToolsFor,
    disabledToolsSettingsKey,
    normalizeToolName,
    withToolAdded,
    withToolRemoved,
  } from '../../utils/promptOverrides';
  import { isImeComposingEvent } from '../../utils/imeComposition';
  import type { ProviderDefinition } from '../../providers/catalog';
  import SettingsField from './SettingsField.svelte';
  import { GHOST_BUTTON_CLASS, INPUT_CLASS, PRIMARY_BUTTON_CLASS } from './styles';

  let { provider }: { provider: ProviderDefinition } = $props();

  let settings = $derived(getSettings());
  let tools = $derived(disabledToolsFor(settings, provider.id));

  let draft = $state('');
  let normalizedDraft = $derived(normalizeToolName(draft));
  let duplicate = $derived(normalizedDraft !== '' && tools.includes(normalizedDraft));
  // The backend refuses a whole UpdateSettings patch over one malformed
  // name, and the caller sees only "Failed to save setting" with the list
  // rolled back under it. Saying which rule the draft breaks, before the
  // write, is the only way the user can act on it.
  let nameError = $derived(disabledToolNameError(draft));
  let canAdd = $derived(normalizedDraft !== '' && !duplicate && nameError === null);
  // One line under the field, whichever refusal applies. A malformed name
  // cannot also be a duplicate — every stored name already passed the same
  // rules — so the order only decides which reads first, not which is lost.
  let draftError = $derived(
    duplicate ? `${normalizedDraft} is already disabled.` : nameError,
  );
  let suggestions = $derived(CLAUDE_TOOL_SUGGESTIONS.filter((name) => !tools.includes(name)));

  async function write(next: string[]): Promise<void> {
    const key = disabledToolsSettingsKey(provider.id);
    if (!key) return;
    await updateSettingsPatch({ [key]: next });
  }

  function addTool(name: string): void {
    // The guard lives here rather than only in the draft path, so no caller
    // can put a name the backend would reject on the wire. Reaching it from
    // the suggestion row would mean a bad literal in CLAUDE_TOOL_SUGGESTIONS
    // — our bug, and one that must not be silent.
    const error = disabledToolNameError(name);
    if (error) {
      console.error(`Refusing to disable ${JSON.stringify(name)}: ${error}`);
      return;
    }
    const next = withToolAdded(tools, name);
    if (next.length === tools.length) return;
    void write(next);
  }

  function addDraft(): void {
    if (!canAdd) return;
    addTool(normalizedDraft);
    draft = '';
  }

  function removeTool(name: string): void {
    void write(withToolRemoved(tools, name));
  }

  function handleKeydown(e: KeyboardEvent): void {
    if (e.key !== 'Enter' || isImeComposingEvent(e)) return;
    e.preventDefault();
    addDraft();
  }
</script>

<div data-testid="settings-claude-tools-{provider.id}">
  <SettingsField
    label="Disabled tools"
    hint="Their schemas never reach the model. Names are passed to the {provider.cliLabel} verbatim, so a name it doesn't recognise is harmless."
    align="start"
    stacked
  >
    <div class="flex flex-col gap-2.5">
      {#if tools.length === 0}
        <p class="text-[0.71875rem] text-fg-hint" data-testid="settings-claude-tools-empty">
          No tools disabled.
        </p>
      {:else}
        <div class="flex flex-wrap gap-1.5" data-testid="settings-claude-tools-list">
          {#each tools as tool (tool)}
            <span
              class="inline-flex items-center gap-1.5 rounded-[var(--radius-field)] border border-border-subtle bg-surface-0 py-0.5 pl-2 pr-1 font-mono text-[0.6875rem] text-fg-muted"
              data-testid="settings-claude-tool-{tool}"
            >
              {tool}
              <button
                type="button"
                class="cursor-pointer rounded-[var(--radius-field)] px-1 text-fg-hint transition-colors hover:text-fg focus:outline-none focus-visible:ring-2 focus-visible:ring-accent/40"
                data-testid="settings-claude-tool-remove-{tool}"
                aria-label="Re-enable {tool}"
                onclick={() => removeTool(tool)}
              >
                ×
              </button>
            </span>
          {/each}
        </div>
      {/if}

      <div class="flex items-start gap-2">
        <input
          type="text"
          data-testid="settings-claude-tool-input"
          value={draft}
          placeholder="Tool name"
          autocomplete="off"
          spellcheck="false"
          aria-label="Tool name to disable"
          aria-invalid={draftError !== null}
          oninput={(e) => (draft = (e.currentTarget as HTMLInputElement).value)}
          onkeydown={handleKeydown}
          class="{INPUT_CLASS} max-w-[14rem] font-mono"
        />
        <button
          type="button"
          data-testid="settings-claude-tool-add"
          class={PRIMARY_BUTTON_CLASS}
          disabled={!canAdd}
          onclick={addDraft}
        >
          Add
        </button>
      </div>

      {#if draftError}
        <p class="text-[0.71875rem] text-error" role="alert" data-testid="settings-claude-tool-error">
          {draftError}
        </p>
      {/if}

      {#if suggestions.length > 0}
        <div class="flex flex-wrap items-center gap-1.5">
          <span class="text-[0.6875rem] text-fg-hint">Common:</span>
          {#each suggestions as name (name)}
            <button
              type="button"
              class={GHOST_BUTTON_CLASS + ' font-mono'}
              data-testid="settings-claude-tool-suggest-{name}"
              onclick={() => addTool(name)}
            >
              {name}
            </button>
          {/each}
        </div>
      {/if}
    </div>
  </SettingsField>
</div>
