<script lang="ts">
  // One system-prompt override: enable switch, the file on the computer that
  // holds the prompt, and the models it applies to. The text is edited in
  // the file, never here. Purely presentational: every mutation and the open
  // go back to ProviderPromptSection, which owns the list and the settings
  // write.

  import FilePen from '@lucide/svelte/icons/file-pen';
  import Icon from '../primitives/Icon.svelte';
  import ToggleSwitch from '../shared/ToggleSwitch.svelte';
  import PromptModelSelectChips from './PromptModelSelectChips.svelte';
  import { GHOST_BUTTON_CLASS, SECONDARY_BUTTON_CLASS } from './styles';
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
    onOpenFile,
  }: {
    provider: ProviderID;
    entry: PromptOverride;
    index: number;
    shadowed: string[];
    models: ModelInfo[];
    onPatch: (patch: Partial<PromptOverride>) => void;
    onRemove: () => void;
    /** Opens the entry's prompt file in the editor; null hides the control
     *  (the page cannot act on the computer's desktop). */
    onOpenFile: ((path: string) => void) | null;
  } = $props();

  // Each warning sits under the control that causes it: an empty model list
  // and a blank file are independent mistakes, and one merged message would
  // point at the wrong control half the time.
  let inert = $derived(entryInertness(entry));
  let shadowedLabels = $derived(
    shadowed.map((slug) => displayModelLabel(provider, slug)).join(', '),
  );
  let fileName = $derived(entry.path ? entry.path.slice(Math.max(entry.path.lastIndexOf('/'), entry.path.lastIndexOf('\\')) + 1) : '');
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
    <p class="text-[0.71875rem] text-fg-muted">Prompt file</p>
    {#if entry.path}
      <div class="flex min-w-0 items-center gap-2" data-testid="settings-prompt-file-{provider}-{index}">
        <!-- Truncated from the left: the file name is the end worth
             keeping. The inner span keeps the path's own direction. -->
        <span
          class="min-w-0 flex-1 truncate text-left font-mono text-[0.71875rem] text-fg"
          dir="rtl"
          title={entry.path}
          data-testid="settings-prompt-path-{provider}-{index}"
        ><span dir="ltr">{entry.path}</span></span>
        {#if onOpenFile}
          <button
            type="button"
            class="{SECONDARY_BUTTON_CLASS} inline-flex shrink-0 items-center gap-1.5"
            data-testid="settings-prompt-open-{provider}-{index}"
            aria-label="Open {fileName} in editor"
            onclick={() => onOpenFile(entry.path ?? '')}
          >
            <Icon icon={FilePen} size={12} strokeWidth={2} />
            Edit file
          </button>
        {/if}
      </div>
    {:else}
      <p class="text-[0.71875rem] text-fg-hint" data-testid="settings-prompt-unsaved-{provider}-{index}">
        Saving…
      </p>
    {/if}
    {#if entry.problem}
      <p
        class="text-[0.71875rem] text-warning"
        data-testid="settings-prompt-problem-{provider}-{index}"
      >
        {entry.problem}
      </p>
    {:else if inert.blankFile}
      <p
        class="text-[0.71875rem] text-warning"
        data-testid="settings-prompt-blank-{provider}-{index}"
      >
        The file is empty, so this override is not applied. Write the prompt there or turn it off.
      </p>
    {/if}
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
        Not applied. Select at least one model.
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
</div>
