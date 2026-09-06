<script lang="ts">
  import type { AssetKind } from '../../stores/frontendAssets';
  import { copyAppearanceFiles, usesFrontendAssetLibrary } from '../../stores/appearanceFiles';
  import { backendDisplayName, backendReachable, getAttachedBackends } from '../../stores/attachedBackends.svelte';
  import { selectedBackend } from '../../stores/selectedBackend.svelte';
  import { hasScope } from '../../transport/scopes';
  import { userFacingError } from '../../utils/userFacingError';
  import SettingsField from './SettingsField.svelte';
  import SettingsCallout from './SettingsCallout.svelte';
  import { SELECT_CLASS, SECONDARY_BUTTON_CLASS } from './styles';

  let { kind }: { kind: AssetKind } = $props();
  let chosen = $state<string | null>(null);
  let busy = $state(false);
  let error = $state('');
  let copied = $state(false);
  const label = $derived(kind === 'themes' ? 'Copy themes' : 'Copy animations');
  const computers = $derived(getAttachedBackends());
  const selected = $derived(chosen ?? (computers.some((computer) => computer.id === selectedBackend()) ? selectedBackend() : computers[0]?.id ?? null));
  const available = $derived(selected !== null && computers.some((computer) => computer.id === selected) && backendReachable(selected) && hasScope('settings:read', selected));

  async function copy(): Promise<void> {
    if (!available || selected === null || busy) return;
    const source = selected;
    busy = true;
    error = '';
    copied = false;
    try {
      await copyAppearanceFiles(kind, source);
      copied = true;
    } catch (cause) { error = userFacingError(cause); }
    finally { busy = false; }
  }
</script>

{#if usesFrontendAssetLibrary()}
  <SettingsField
    id={kind === 'themes' ? 'theme.copy-files' : 'spinner.copy-files'}
    {label}
    hint="Replace this device’s custom files with a copy from a computer. Your selections stay the same."
    stacked
  >
    <div class="flex min-w-0 flex-wrap items-center justify-end gap-2">
      <select aria-label={`${label} from computer`} class={`${SELECT_CLASS} min-w-0 max-w-full flex-1`} disabled={busy || computers.length === 0}
        value={selected ?? ''} onchange={(event) => { chosen = event.currentTarget.value; copied = false; error = ''; }}>
        {#if computers.length === 0}<option value="">No computers connected</option>{/if}
        {#each computers as computer (computer.id)}
          <option value={computer.id}>{backendDisplayName(computer)}{backendReachable(computer.id) ? '' : ' (offline)'}</option>
        {/each}
      </select>
      <button type="button" class={SECONDARY_BUTTON_CLASS}
        disabled={busy || !available} onclick={copy}>{busy ? 'Copying…' : 'Copy'}</button>
    </div>
  </SettingsField>
  {#if copied}<p class="px-1 text-xs text-fg-muted" role="status">{kind === 'themes' ? 'Themes copied.' : 'Animations copied.'}</p>{/if}
  {#if error}<SettingsCallout tone="error">{error}</SettingsCallout>{/if}
{/if}
