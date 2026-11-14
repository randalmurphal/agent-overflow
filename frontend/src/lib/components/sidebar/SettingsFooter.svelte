<script lang="ts">
  // Bottom-of-sidebar settings launcher. Extracted from the monolithic
  // Sidebar.svelte so the shell stays a layout-only file.
  import Settings from '@lucide/svelte/icons/settings';
  import Moon from '@lucide/svelte/icons/moon';
  import Sun from '@lucide/svelte/icons/sun';
  import Volume2 from '@lucide/svelte/icons/volume-2';
  import VolumeX from '@lucide/svelte/icons/volume-x';
  import Icon from '../primitives/Icon.svelte';
  import Button from '../primitives/Button.svelte';
  import { hasPendingUpdate } from '../../stores/updates.svelte';
  import { openSettingsOverlay } from '../../stores/settingsOverlay.svelte';
  import { getSettings, updateSetting } from '../../stores/settings.svelte';
  import { hasScope, isViewOnly } from '../../transport/scopes';
  import { isFrontendOnly } from '../../transport/runMode';

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
  let noHost = $derived(isFrontendOnly() || !hasScope('host'));
  // The app's one ambient read-only marker. It rides the footer the
  // sidebar already draws rather than a banner of its own: the mode is a
  // standing fact about this session, not an event, and a surface that
  // reserves space at the top of the shell for a standing fact is the
  // opposite of unobtrusive. Never rendered on the owner's own screen or
  // for a full-access device — see transport/scopes.ts isViewOnly.
  let viewOnly = $derived(isViewOnly());
  let keepAwakeTitle = $derived(
    keepAwakeOn
      ? settings.keepAwakeScreen
        ? 'Keep awake is on (machine + screen). Click to allow sleep.'
        : 'Keep awake is on (machine only). Click to allow sleep.'
      : 'Keep the machine awake',
  );
  // notificationSoundsEnabled is DEVICE tier (internal/settings/tier.go):
  // it decides whether this screen makes noise, not whether the machine
  // notifies. So unlike keep-awake above it carries no host gate — a
  // view-only or remote session owns its own speaker and gets the switch.
  let soundsOn = $derived(settings.notificationSoundsEnabled);
  let updatePending = $derived(hasPendingUpdate());
</script>

{#if onOpenSettings}
  <div class="@container border-t border-border-subtle p-2 shrink-0 flex items-center gap-1">
    <Button
      variant="ghost"
      size="sm"
      onclick={onOpenSettings}
      testId="sidebar-settings-button"
      title="Settings"
      ariaLabel="Settings"
      class="flex-1 min-w-0 justify-start"
    >
      {#snippet leading()}
        <Icon icon={Settings} size={13} strokeWidth={2} class="opacity-80 shrink-0" />
      {/snippet}
      {#snippet children()}
        <!-- The row cannot hold both labels on a narrow sidebar, so Settings
             keeps its icon and gives its label up to the Update button. -->
        <span data-testid="sidebar-settings-label" class={updatePending ? '@max-[15rem]:hidden' : ''}>Settings</span>
      {/snippet}
    </Button>
    {#if updatePending}
      <Button
        variant="tinted"
        size="sm"
        onclick={() => openSettingsOverlay('updates')}
        testId="sidebar-update-button"
        title="A new version is available. Open Updates."
        class="shrink-0"
      >
        {#snippet children()}
          Update
        {/snippet}
      </Button>
    {/if}
    {#if viewOnly}
      <span
        class="shrink-0 rounded-[var(--radius-field)] border border-border-subtle px-1.5 py-0.5 text-[0.625rem] font-medium uppercase tracking-[0.12em] text-fg-subtle"
        data-testid="view-only-indicator"
        title="This device was paired with read-only access."
      >View only</span>
    {/if}
    <Button
      variant="ghost"
      size="sm"
      pressed={!soundsOn}
      onclick={() => updateSetting('notificationSoundsEnabled', !soundsOn)}
      testId="sidebar-sound-toggle"
      title={soundsOn
        ? 'Notification sounds on for this screen. Click to mute.'
        : 'Notification sounds muted on this screen. Click to unmute.'}
      ariaLabel={soundsOn ? 'Mute notification sounds' : 'Unmute notification sounds'}
      class="px-0 w-7 justify-center shrink-0"
    >
      {#snippet children()}
        {#if soundsOn}
          <Icon icon={Volume2} size={13} strokeWidth={2} class="opacity-80" />
        {:else}
          <Icon icon={VolumeX} size={13} strokeWidth={2} class="text-accent" />
        {/if}
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
