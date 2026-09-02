<script lang="ts">
  // SettingsHeader: standard section header used by every settings tab.
  //
  // The pattern is:
  //   title (15px semibold, a step above the 13px field label so a section
  //   never reads as one more row) + optional inline status badge on the right
  //   optional one-line description
  //   optional `details` prose, for the few sections whose explanation
  //   needs inline markup (a <code> span) and so cannot be a string
  //
  // Centralizing it keeps every tab's typography rhythm identical and
  // removes the per-section boilerplate that used to repeat the same
  // head four or five times in this folder.
  //
  // The header also owns the gap to the content below it (`mb-3`), so
  // sections don't each hand-spread the same top margin onto whatever
  // happens to come first. `last:mb-0` drops it for the degenerate
  // sections that render a header and nothing else (a client-mode
  // placeholder), where a trailing margin would only widen the gap to
  // the next section. The gap ABOVE a section is the page's business:
  // `.settings-sections` (app.css) rules and spaces between siblings.

  import type { Snippet } from 'svelte';
  import { SECTION_PROSE_CLASS } from './styles';

  let {
    title,
    description,
    details,
    badge,
  }: {
    title: string;
    description?: string;
    details?: Snippet;
    badge?: Snippet;
  } = $props();
</script>

<header class="mb-3 flex flex-col gap-0.5 last:mb-0">
  <div class="flex items-baseline gap-2.5">
    <h3 class="text-[0.9375rem] font-semibold tracking-tight text-fg">{title}</h3>
    {#if badge}
      <span class="ml-auto">{@render badge()}</span>
    {/if}
  </div>
  {#if description}
    <p class={SECTION_PROSE_CLASS}>
      {description}
    </p>
  {/if}
  {#if details}
    <p class={SECTION_PROSE_CLASS}>{@render details()}</p>
  {/if}
</header>
