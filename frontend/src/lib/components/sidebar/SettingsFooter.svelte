<script lang="ts">
  // Bottom-of-sidebar settings launcher. Extracted from the monolithic
  // Sidebar.svelte so the shell stays a layout-only file.
  import Settings from 'lucide-svelte/icons/settings';
  import Icon from '../primitives/Icon.svelte';
  import Button from '../primitives/Button.svelte';

  interface Props {
    onOpenSettings?: () => void;
  }
  let { onOpenSettings }: Props = $props();
</script>

{#if onOpenSettings}
  <div class="border-t border-border-subtle p-2 shrink-0">
    <Button
      variant="ghost"
      size="sm"
      onclick={onOpenSettings}
      testId="sidebar-settings-button"
      class="w-full justify-start"
    >
      {#snippet leading()}
        <Icon icon={Settings} size={13} strokeWidth={2} class="opacity-80" />
      {/snippet}
      {#snippet children()}Settings{/snippet}
    </Button>
  </div>
{/if}
