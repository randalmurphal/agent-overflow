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

  import type { Snippet } from 'svelte';

  let {
    label,
    hint,
    htmlFor,
    stacked = false,
    align = 'center',
    children,
  }: {
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
  <div class="flex flex-col gap-2 py-1.5">
    <div class="flex flex-col gap-0.5">
      {#if htmlFor}
        <label for={htmlFor} class="text-[13px] font-medium text-fg">{label}</label>
      {:else}
        <p class="text-[13px] font-medium text-fg">{label}</p>
      {/if}
      {#if hint}
        <p class="text-[11.5px] leading-snug text-fg-muted">{hint}</p>
      {/if}
    </div>
    <div>{@render children()}</div>
  </div>
{:else}
  <div class="flex {alignClass} justify-between gap-4 py-1.5">
    <div class="min-w-0 flex-1">
      {#if htmlFor}
        <label for={htmlFor} class="block text-[13px] font-medium text-fg">{label}</label>
      {:else}
        <p class="text-[13px] font-medium text-fg">{label}</p>
      {/if}
      {#if hint}
        <p class="mt-0.5 text-[11.5px] leading-snug text-fg-muted">{hint}</p>
      {/if}
    </div>
    <div class="shrink-0 max-w-[60%]">{@render children()}</div>
  </div>
{/if}
