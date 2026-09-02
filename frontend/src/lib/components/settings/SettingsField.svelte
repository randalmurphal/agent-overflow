<script lang="ts">
  // SettingsField: one labeled control row.
  //
  // Replaces the old `flex items-center justify-between gap-4 py-2.5`
  // pattern that every section was hand-rolling, plus the `divide-y`
  // around groups of those rows. The field stack relies on vertical
  // rhythm (parent applies `space-y-*`) instead of borders, which reads
  // calmer on dark surfaces and stops the page from looking like a
  // spreadsheet.
  //
  // Layout: label + optional hint on the left; control(s) on the right.
  // For controls that need full width below the label (sliders, large
  // inputs), pass `stacked`.
  //
  // `id` is the field's entry in the search index (`fields.ts`). It is
  // required so a control cannot be added without becoming searchable, and
  // typed against the index so a typo is a compile error. The root carries
  // it as `data-settings-field` — the hook search scrolls to and flashes —
  // and the label and hint as `data-settings-label` / `data-settings-hint`,
  // which `fields.test.ts` compares against the index so the two cannot
  // drift.

  import type { Snippet } from 'svelte';
  import type { SettingsFieldId } from './fields';

  let {
    id,
    label,
    hint,
    htmlFor,
    stacked = false,
    align = 'center',
    children,
  }: {
    id: SettingsFieldId;
    label: string;
    hint?: string;
    htmlFor?: string;
    stacked?: boolean;
    align?: 'center' | 'start';
    children: Snippet;
  } = $props();

  let alignClass = $derived(align === 'start' ? 'items-start' : 'items-center');
</script>

{#if stacked}
  <div
    class="flex flex-col gap-2 rounded-[var(--radius-field)] py-1.5"
    data-settings-field={id}
    data-settings-label={label}
    data-settings-hint={hint}
  >
    <div class="flex flex-col gap-0.5">
      {#if htmlFor}
        <label for={htmlFor} class="text-[0.8125rem] font-medium text-fg">{label}</label>
      {:else}
        <p class="text-[0.8125rem] font-medium text-fg">{label}</p>
      {/if}
      {#if hint}
        <p class="text-[0.71875rem] leading-snug text-fg-muted">{hint}</p>
      {/if}
    </div>
    <div>{@render children()}</div>
  </div>
{:else}
  <div
    class="flex {alignClass} justify-between gap-4 rounded-[var(--radius-field)] py-1.5"
    data-settings-field={id}
    data-settings-label={label}
    data-settings-hint={hint}
  >
    <div class="min-w-0 flex-1">
      {#if htmlFor}
        <label for={htmlFor} class="block text-[0.8125rem] font-medium text-fg">{label}</label>
      {:else}
        <p class="text-[0.8125rem] font-medium text-fg">{label}</p>
      {/if}
      {#if hint}
        <p class="mt-0.5 text-[0.71875rem] leading-snug text-fg-muted">{hint}</p>
      {/if}
    </div>
    <div class="shrink-0 max-w-[60%]">{@render children()}</div>
  </div>
{/if}
