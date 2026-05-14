<script lang="ts">
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
{:else}
  <ClaudeIcon {size} class="text-[#d97757] opacity-95 {className}" />
{/if}
