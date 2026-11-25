<script lang="ts">
  import AntigravityIcon from '../primitives/brand/AntigravityIcon.svelte';
  import ClaudeIcon from '../primitives/brand/ClaudeIcon.svelte';
  import OpenAIIcon from '../primitives/brand/OpenAIIcon.svelte';
  import { asProviderID } from '../../types/providers';

  let {
    provider,
    size = 13,
    class: className = '',
  }: {
    provider: string | null | undefined;
    size?: number;
    class?: string;
  } = $props();

  let providerID = $derived(asProviderID(provider));
</script>

{#if providerID === 'codex'}
  <OpenAIIcon {size} class="opacity-95 {className}" />
{:else if providerID === 'claude-tui'}
  <!-- claude-tui shares Claude's glyph but in terminal green, marking it as the
       live-TUI provider at a glance. No square chip: on the app's black terminal
       surface a boxed/outlined glyph reads as a muddy hollow shape at icon sizes,
       so the solid green starburst is cleaner and unmistakably Claude. -->
  <ClaudeIcon {size} class="text-provider-claude-tui opacity-95 {className}" />
{:else if providerID === 'claude'}
  <ClaudeIcon {size} class="text-provider-claude opacity-95 {className}" />
{:else if providerID === 'agy'}
  <AntigravityIcon {size} class="opacity-95 {className}" />
{:else}
  <span
    class="inline-flex items-center justify-center rounded-sm bg-surface-2 text-[0.625rem] font-semibold uppercase text-fg-muted {className}"
    style="width: {size}px; height: {size}px;"
    aria-hidden="true"
  >{(provider ?? '?').slice(0, 1)}</span>
{/if}
