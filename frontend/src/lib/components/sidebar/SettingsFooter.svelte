<script lang="ts">
  // Bottom-of-sidebar settings launcher. Extracted from the monolithic
  // Sidebar.svelte so the shell stays a layout-only file.
  import Settings from '@lucide/svelte/icons/settings';
  import Moon from '@lucide/svelte/icons/moon';
  import Sun from '@lucide/svelte/icons/sun';
  import Icon from '../primitives/Icon.svelte';
  import Button from '../primitives/Button.svelte';
  import UpdateBadge from '../shared/UpdateBadge.svelte';
  import { hasPendingUpdate } from '../../stores/updates.svelte';
  import { getSettings, updateSetting } from '../../stores/settings.svelte';
  import { hasScope } from '../../transport/scopes';

  interface Props {
    onOpenSettings?: () => void;
  }
  let { onOpenSettings }: Props = $props();

  let settings = $derived(getSettings());
  let keepAwakeOn = $derived(settings.keepAwakeEnabled);
  // Keep-awake asserts OS power state on the machine running the app —
  // a desktop-host control, so a view-only (--connect) session hides it
  // rather than offering a switch the host-scoped RPC would refuse.
  // keepAwakeEnabled is a host-tier settings key (internal/settings/tier.go):
  // it drives THIS machine's sleep inhibitor, so it is host presence that
  // authorizes it rather than any grant.
  let noHost = $derived(!hasScope('host'));
  let keepAwakeTitle = $derived(
    keepAwakeOn
      ? settings.keepAwakeScreen
        ? 'Keep awake is on (machine + screen). Click to allow sleep.'
        : 'Keep awake is on (machine only). Click to allow sleep.'
      : 'Keep the machine awake',
  );
</script>

{#if onOpenSettings}
  <div class="border-t border-border-subtle p-2 shrink-0 flex items-center gap-1">
    <Button
      variant="ghost"
      size="sm"
      onclick={onOpenSettings}
      testId="sidebar-settings-button"
      class="flex-1 min-w-0 justify-start"
    >
      {#snippet leading()}
        <Icon icon={Settings} size={13} strokeWidth={2} class="opacity-80" />
      {/snippet}
      {#snippet children()}
        Settings{#if hasPendingUpdate()}<UpdateBadge />{/if}
      {/snippet}
    </Button>
    {#if !noHost}
      <Button
        variant="ghost"
        size="sm"
        pressed={keepAwakeOn}
        onclick={() => updateSetting('keepAwakeEnabled', !keepAwakeOn)}
        testId="sidebar-keep-awake-toggle"
        title={keepAwakeTitle}
        ariaLabel={keepAwakeOn ? 'Disable keep awake' : 'Enable keep awake'}
        class="px-0 w-7 justify-center shrink-0"
      >
        {#snippet children()}
          {#if keepAwakeOn}
            <Icon icon={Sun} size={13} strokeWidth={2} class="text-accent" />
          {:else}
            <Icon icon={Moon} size={13} strokeWidth={2} class="opacity-80" />
          {/if}
        {/snippet}
      </Button>
    {/if}
  </div>
{/if}
