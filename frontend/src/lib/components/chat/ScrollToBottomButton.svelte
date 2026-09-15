<script lang="ts">
  import ChevronDown from '@lucide/svelte/icons/chevron-down';
  import { fade } from 'svelte/transition';
  import Button from '../primitives/Button.svelte';
  import Icon from '../primitives/Icon.svelte';

  interface Props {
    visible: boolean;
    onClick: () => void;
  }

  let { visible, onClick }: Props = $props();
</script>

{#if visible}
  <!-- Chat publishes its overlay height; discussion has a sibling composer. -->
  <div
    transition:fade={{ duration: 120 }}
    style="bottom: calc(var(--composer-height, 0px) + 0.5rem);"
    class="absolute inset-x-0 z-30 mx-auto flex w-max rounded-full bg-card shadow-sheet"
  >
    <Button
      onclick={onClick}
      testId="scroll-to-bottom"
      size="sm"
      class="rounded-full! whitespace-nowrap"
    >
      Jump to bottom
      {#snippet trailing()}
        <Icon icon={ChevronDown} size={14} strokeWidth={2} />
      {/snippet}
    </Button>
  </div>
{/if}
