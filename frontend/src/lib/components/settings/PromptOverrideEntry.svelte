<script lang="ts">
  // One system-prompt override: enable switch, the models it applies to, and
  // the prompt itself. Purely presentational — every mutation goes back to
  // ProviderPromptSection, which owns the list and the settings write.
  //
  // The prompt commits on `change`, i.e. when the textarea loses focus with a
  // changed value. Blur precedes the click of whichever control took focus,
  // so clicking Remove / Add / a chip commits the typing first rather than
  // discarding it.

  import ToggleSwitch from '../shared/ToggleSwitch.svelte';
  import PromptModelSelectChips from './PromptModelSelectChips.svelte';
  import { CONTROL_BASE, GHOST_BUTTON_CLASS } from './styles';
  import { displayModelLabel } from '../../utils/modelLabels';
  import { entryInertness, toggleModelSelection } from '../../utils/promptOverrides';
  import type { ProviderID } from '../../providers/catalog';
  import type { ModelInfo, PromptOverride } from '../../types/settings';

  let {
    provider,
    entry,
    index,
    shadowed,
    models,
    onPatch,
    onRemove,
    onEditorFocus,
  }: {
    provider: ProviderID;
    entry: PromptOverride;
    index: number;
    shadowed: string[];
    models: ModelInfo[];
    onPatch: (patch: Partial<PromptOverride>) => void;
    onRemove: () => void;
    onEditorFocus: (el: HTMLTextAreaElement, index: number) => void;
  } = $props();

  let idBase = $derived(`prompt-override-${provider}-${index}`);
  // Two ways an entry reads as active while doing nothing, and each warning
  // sits under the field that causes it — an empty model list and an empty
  // prompt are independent mistakes, and one merged message next to the
  // chips would point at the wrong control half the time.
  let inert = $derived(entryInertness(entry));
  let shadowedLabels = $derived(
    shadowed.map((slug) => displayModelLabel(provider, slug)).join(', '),
  );

  function commitPrompt(value: string): void {
    if (value === entry.prompt) return;
    onPatch({ prompt: value });
  }
</script>

<div
  class="rounded-[var(--radius-control)] border border-border-subtle/70 bg-surface-0/40 px-3.5 py-3"
  data-testid="settings-prompt-entry-{provider}-{index}"
  data-enabled={entry.enabled}
>
  <div class="flex items-center gap-3">
    <span class="text-[0.71875rem] font-medium uppercase tracking-[0.14em] text-fg-hint">
      Override {index + 1}
    </span>
    <span class="ml-auto" data-testid="settings-prompt-enabled-{provider}-{index}">
      <ToggleSwitch
        checked={entry.enabled}
        ariaLabel="Enable {provider} override {index + 1}"
        onToggle={(value) => onPatch({ enabled: value })}
      />
    </span>
    <button
      type="button"
      class={GHOST_BUTTON_CLASS}
      data-testid="settings-prompt-remove-{provider}-{index}"
      aria-label="Remove {provider} override {index + 1}"
      onclick={onRemove}
    >
      Remove
    </button>
  </div>

  <div class="mt-2.5 flex flex-col gap-1.5">
    <p class="text-[0.71875rem] text-fg-muted">Applies to</p>
    <PromptModelSelectChips
      {provider}
      {index}
      {models}
      selected={entry.models}
      onToggle={(slug) => onPatch({ models: toggleModelSelection(entry.models, slug) })}
    />
    {#if inert.noModels}
      <p
        class="text-[0.71875rem] text-warning"
        data-testid="settings-prompt-nomodels-{provider}-{index}"
      >
        Not applied — select at least one model.
      </p>
    {/if}
    {#if shadowed.length > 0}
      <p
        class="text-[0.71875rem] text-warning"
        data-testid="settings-prompt-shadowed-{provider}-{index}"
      >
        An earlier enabled override already covers {shadowedLabels}.
      </p>
    {/if}
  </div>

  <div class="mt-2.5 flex flex-col gap-1.5">
    <label for={idBase} class="text-[0.71875rem] text-fg-muted">Prompt</label>
    <textarea
      id={idBase}
      data-prompt-editor
      data-testid="settings-prompt-text-{provider}-{index}"
      rows={8}
      spellcheck="false"
      value={entry.prompt}
      onfocus={(e) => onEditorFocus(e.currentTarget as HTMLTextAreaElement, index)}
      onchange={(e) => commitPrompt((e.currentTarget as HTMLTextAreaElement).value)}
      placeholder="Replaces the default system prompt for the selected models."
      class="{CONTROL_BASE} min-h-[9rem] w-full resize-y px-2.5 py-2 font-mono text-[0.75rem] leading-relaxed"
    ></textarea>
    {#if inert.noPrompt}
      <p
        class="text-[0.71875rem] text-warning"
        data-testid="settings-prompt-noprompt-{provider}-{index}"
      >
        Not applied — write a prompt, or turn the override off.
      </p>
    {/if}
  </div>
</div>
