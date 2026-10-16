<script lang="ts">
  // Settings → Performance → "Disable GPU vsync": the Windows launcher's
  // --disable-gpu-vsync preference. It lives in the launcher's wsl.json
  // beside the distro choice (not in the settings store) because the
  // launcher reads it before any backend runs; the launcher applies it to
  // the app window and the browser pane on its next start.
  //
  // Reading it is a fact about the Windows machine, so like WSLSection the
  // read is `host` scoped, which no paired device holds: an ungranted page
  // asks nothing and renders nothing (passiveLoads.test.ts), and so does a
  // backend that is not running under the launcher.
  import {
    GetWebviewGPUVsyncDisabled,
    IsWSL,
    SetWebviewGPUVsyncDisabled,
  } from '../../stores/bindings';
  import { addErrorToast } from '../../stores/toast.svelte';
  import { errString } from '../../utils/errors';
  import { isClientMode } from '../../transport/runMode';
  import { untrack } from 'svelte';
  import ToggleSwitch from '../shared/ToggleSwitch.svelte';
  import SettingsCallout from './SettingsCallout.svelte';
  import SettingsField from './SettingsField.svelte';
  import { settingsComputer } from './settingsComputer';

  const { call, hasScope } = settingsComputer();
  const clientMode = isClientMode();
  let localOnly = $derived(clientMode || !hasScope('host'));

  // The launcher started with the value it read at launch, which is the
  // value this page finds on mount; a saved value that differs from it is
  // waiting on a restart. Nothing on the wire reports the launcher's own
  // flags, so the mount value is the witness (the same reasoning as the
  // tracing toggle in ObservabilitySettings).
  let supported = $state(false);
  let launchedWith = $state(false);
  let saved = $state(false);
  let saving = $state(false);
  let restartPending = $derived(supported && saved !== launchedWith);

  async function load(): Promise<void> {
    try {
      if (!(await call(() => IsWSL()))) return;
      const value = !!(await call(() => GetWebviewGPUVsyncDisabled()));
      launchedWith = value;
      saved = value;
      supported = true;
    } catch (err) {
      addErrorToast(`Failed to read the GPU vsync preference: ${errString(err)}`, err);
    }
  }

  async function toggle(value: boolean): Promise<void> {
    if (saving || value === saved) return;
    saving = true;
    try {
      // Reconciled from the write's answer, never assumed.
      saved = !!(await call(() => SetWebviewGPUVsyncDisabled(value)));
    } catch (err) {
      addErrorToast(`Failed to save the GPU vsync preference: ${errString(err)}`, err);
    } finally {
      saving = false;
    }
  }

  // The gate is the only tracked read. The load itself runs untracked: a
  // binding that fails synchronously would otherwise toast inside this
  // effect's own run and re-trigger it forever.
  $effect(() => {
    if (localOnly) return;
    untrack(() => void load());
  });
</script>

{#if supported}
  <SettingsField
    id="performance.disable-gpu-vsync"
    label="Disable GPU vsync"
    hint="Pace frames from a timer instead of the display's vblank signal. Fixes a periodic hitch in scrolling and streaming text on Windows when monitors run at different refresh rates. Applies on next launch."
  >
    <ToggleSwitch
      checked={saved}
      disabled={saving}
      ariaLabel="Toggle Disable GPU vsync"
      onToggle={(value) => void toggle(value)}
    />
  </SettingsField>
  {#if restartPending}
    <SettingsCallout tone="warn">
      GPU vsync changes take effect the next time you open Agent Overflow.
    </SettingsCallout>
  {/if}
{/if}
