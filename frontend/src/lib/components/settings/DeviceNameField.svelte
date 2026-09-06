<script lang="ts">
  import { untrack } from 'svelte';
  import { GetDeviceName, SetDeviceName } from '../../stores/bindings';
  import { clientDeviceName, clientDeviceNameStatus, saveClientDeviceName } from '../../stores/clientDeviceName.svelte';
  import { isNativeShell } from '../../native/platform';
  import { hasPairedSession } from '../../transport/deviceSession';
  import { HOME_BACKEND, type BackendKey } from '../../transport/backendKey';
  import { withBackendTarget } from '../../transport/backends';
  import { backendHasCapability, getTransportHelloFor } from '../../stores/transportStatus.svelte';
  import { hasScope } from '../../transport/scopes';
  import { errString } from '../../utils/errors';
  import { onDeviceNameChanged } from '../../stores/deviceNames';
  import SettingsField from './SettingsField.svelte';
  import Button from '../primitives/Button.svelte';
  import { INPUT_CLASS } from './styles';
  import type { SettingsFieldId } from './fields';

  let { backend, fieldId = 'systems.device-name' }: { backend?: BackendKey; fieldId?: SettingsFieldId } = $props();
  // The settings computer context remounts the page when its target changes.
  // svelte-ignore state_referenced_locally
  const target = backend ?? HOME_BACKEND;

  // Connections is this installation's page. The selected execution computer
  // must never redirect this field to a different machine.
  // svelte-ignore state_referenced_locally
  const clientOwned = backend === undefined && (isNativeShell() || hasPairedSession(HOME_BACKEND));
  let name = $state(clientOwned ? clientDeviceName() : '');
  let baseline = $state(clientOwned ? clientDeviceName() : '');
  let loaded = $state(clientOwned);
  let busy = $state(false);
  let message = $state('');
  let error = $state('');
  let supported = $derived(clientOwned || (target === HOME_BACKEND ? backendHasCapability('device-name.v1') : getTransportHelloFor(target)?.capabilities.includes('device-name.v1')));
  let canEdit = $derived(clientOwned || hasScope('access:admin', target));

  function acceptName(value: string): void {
    if (name === baseline) name = value;
    baseline = value;
  }

  $effect(() => {
    if (!clientOwned) return;
    const value = clientDeviceName();
    untrack(() => acceptName(value));
  });

  $effect(() => {
    if (clientOwned) return;
    return onDeviceNameChanged(target, (value) => {
      acceptName(value);
      error = '';
    }, (err) => { error = errString(err); });
  });

  $effect(() => {
    if (clientOwned || !supported) return;
    let active = true;
    void withBackendTarget(target, () => GetDeviceName()).then((value) => {
      if (active) { acceptName(value); loaded = true; error = ''; }
    }).catch((err) => { if (active) error = errString(err); });
    return () => { active = false; };
  });

  async function save(): Promise<void> {
    if (busy || !loaded || !supported || !canEdit) return;
    busy = true;
    error = message = '';
    try {
      if (clientOwned) {
        saveClientDeviceName(name);
        name = clientDeviceName();
      } else {
        await withBackendTarget(target, () => SetDeviceName(name));
        name = await withBackendTarget(target, () => GetDeviceName());
      }
      baseline = name;
      message = 'Device name saved.';
    } catch (err) { error = errString(err); }
    finally { busy = false; }
  }
</script>

<div class="rounded-[var(--radius-field)] border border-border-subtle bg-surface-0 px-3 py-3">
  <SettingsField id={fieldId} label="Device name" hint="The name this device shares with other computers and phones." htmlFor={fieldId} stacked>
    <form class="flex gap-2" onsubmit={(event) => { event.preventDefault(); void save(); }}>
      <input id={fieldId} class={`${INPUT_CLASS} min-w-0 flex-1`} bind:value={name} disabled={!loaded || !supported || busy || !canEdit} placeholder="Use the default name" />
      <Button type="submit" variant="primary" size="sm" disabled={!loaded || !supported || busy || !canEdit}>Save</Button>
    </form>
    {#if error}<p class="mt-2 text-xs text-error" role="alert">{error}</p>
    {:else if !supported}<p class="mt-2 text-xs text-fg-muted">Update this computer to edit its device name.</p>
    {:else if message || (clientOwned && clientDeviceNameStatus())}<p class="mt-2 text-xs text-fg-muted" role="status">{clientOwned ? clientDeviceNameStatus() || message : message}</p>{/if}
  </SettingsField>
</div>
