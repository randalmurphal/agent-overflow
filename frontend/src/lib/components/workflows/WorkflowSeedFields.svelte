<script lang="ts">
  // A workflow's declared inputs as plain form fields (UI-SPEC §5.1, R2). The
  // word "variables" does not appear: a human filling this in is answering
  // questions the workflow asks, not populating a schema.

  import DirectoryBrowser from '../sidebar/DirectoryBrowser.svelte';
  import type { WorkflowDefinitionInput } from '../../types/workflow';
  import { uniqueEachKeys } from '../../utils/uniqueEachKeys';

  interface Props {
    inputs: readonly WorkflowDefinitionInput[];
    seeds: Record<string, unknown>;
    /** Where a `format: path` field's browser starts. */
    browseRoot: string;
    disabled: boolean;
    onChange: (name: string, value: unknown) => void;
  }
  let { inputs, seeds, browseRoot, disabled, onChange }: Props = $props();

  let browsingFor = $state('');

  const FIELD = 'mt-1 w-full rounded-md border border-border-subtle bg-surface-0 px-2 py-1.5 text-sm text-fg disabled:cursor-not-allowed disabled:opacity-50';

  function text(name: string): string {
    const value = seeds[name];
    return value === undefined || value === null ? '' : String(value);
  }

  // Enum values are hand-authored YAML. The Go validator now refuses
  // duplicates (internal/workflow/def/schema.go, schema.enum), but frozen
  // run snapshots are decoded and never re-validated, so a pre-rule
  // definition can still deliver a repeat — and a repeated key in a keyed
  // `{#each}` throws `each_key_duplicate`, aborting the whole update flush
  // (utils/uniqueEachKeys.ts).
  const enumKeysByInput = $derived.by(() => {
    const map = new Map<string, string[]>();
    for (const input of inputs) {
      if (input.enum?.length) {
        map.set(input.name, uniqueEachKeys(input.enum, (option) => String(option)));
      }
    }
    return map;
  });
</script>

<div class="grid gap-3 sm:grid-cols-2" data-testid="workflow-intake-seeds">
  {#each inputs as input (input.name)}
    {#if input.type === 'boolean'}
      <label class="flex items-center gap-2 text-xs text-fg-muted">
        <input
          type="checkbox"
          checked={seeds[input.name] === true}
          {disabled}
          onchange={(event) => onChange(input.name, event.currentTarget.checked)}
          data-testid={`workflow-seed-${input.name}`}
        />
        {input.name}{#if !input.required}<span class="text-fg-hint">(optional)</span>{/if}
      </label>
    {:else if input.enum}
      <label class="text-xs text-fg-muted">
        {input.name}{#if !input.required} <span class="text-fg-hint">(optional)</span>{/if}
        <select
          class={FIELD}
          value={text(input.name)}
          {disabled}
          onchange={(event) => onChange(input.name, event.currentTarget.value)}
          data-testid={`workflow-seed-${input.name}`}
        >
          <option value="">Choose…</option>
          {#each input.enum as option, optionIndex (enumKeysByInput.get(input.name)?.[optionIndex] ?? optionIndex)}<option value={String(option)}>{String(option)}</option>{/each}
        </select>
      </label>
    {:else if input.multiline}
      <label class="text-xs text-fg-muted sm:col-span-2">
        {input.name}{#if !input.required} <span class="text-fg-hint">(optional)</span>{/if}
        <textarea
          class={`${FIELD} min-h-20`}
          value={text(input.name)}
          {disabled}
          oninput={(event) => onChange(input.name, event.currentTarget.value)}
          data-testid={`workflow-seed-${input.name}`}
        ></textarea>
      </label>
    {:else}
      <label class="text-xs text-fg-muted">
        {input.name}{#if !input.required} <span class="text-fg-hint">(optional)</span>{/if}
        <span class="mt-1 flex gap-1">
          <input
            class={`${FIELD} mt-0 min-w-0 flex-1`}
            type={input.type === 'number' ? 'number' : 'text'}
            value={text(input.name)}
            {disabled}
            oninput={(event) => {
              const raw = event.currentTarget.value;
              onChange(input.name, input.type === 'number' && raw !== '' ? Number(raw) : raw);
            }}
            data-testid={`workflow-seed-${input.name}`}
          />
          {#if input.format === 'path'}
            <button
              type="button"
              class="shrink-0 rounded-md border border-border-subtle px-2 text-xs text-fg-muted hover:text-fg disabled:cursor-not-allowed disabled:opacity-50"
              {disabled}
              onclick={() => (browsingFor = browsingFor === input.name ? '' : input.name)}
              data-testid={`workflow-seed-${input.name}-browse`}
            >Browse</button>
          {/if}
        </span>
      </label>
    {/if}

    {#if browsingFor === input.name}
      <div class="rounded-md border border-border-subtle p-2 sm:col-span-2">
        <DirectoryBrowser
          initialPath={text(input.name) || browseRoot}
          onSelect={(path) => onChange(input.name, path)}
          onSelectFile={(path) => { onChange(input.name, path); browsingFor = ''; }}
        />
      </div>
    {/if}
  {/each}
</div>
