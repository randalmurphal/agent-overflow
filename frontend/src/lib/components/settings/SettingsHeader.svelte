<script lang="ts">
  // SettingsHeader: standard section header used by every settings tab.
  //
  // The pattern is:
  //   eyebrow (uppercase micro-label)
  //   title (15px semibold) + optional inline status badge
  //   one short paragraph describing what the section does
  //
  // Centralizing it keeps every tab's typography rhythm identical and
  // removes the per-section boilerplate that used to repeat the same
  // three-line head four or five times in this folder.

  import type { Snippet } from 'svelte';

  let {
    eyebrow,
    title,
    description,
    badge,
  }: {
    eyebrow?: string;
    title: string;
    description?: string;
    badge?: Snippet;
  } = $props();
</script>

<header class="flex flex-col gap-1">
  <div class="flex items-baseline gap-2.5">
    {#if eyebrow}
      <span class="text-[10.5px] font-medium uppercase tracking-[0.16em] text-fg-hint">
        {eyebrow}
      </span>
    {/if}
    {#if badge}
      <span class="ml-auto">{@render badge()}</span>
    {/if}
  </div>
  <h3 class="text-[15px] font-semibold text-fg leading-snug">{title}</h3>
  {#if description}
    <p class="max-w-2xl text-[12px] leading-relaxed text-fg-muted">
      {description}
    </p>
  {/if}
</header>
