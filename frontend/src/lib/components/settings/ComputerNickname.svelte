<script lang="ts">
  import type { BackendKey } from '../../transport/backendKey';
  import { backendNickname, setBackendNickname } from '../../stores/attachedBackends.svelte';
  import { errString } from '../../utils/errors';
  import Button from '../primitives/Button.svelte';
  import { INPUT_CLASS } from './styles';

  let { backend }: { backend: BackendKey } = $props();
  let editing = $state(false);
  let value = $state('');
  let error = $state('');
  function edit(): void {
    value = backendNickname(backend);
    error = '';
    editing = true;
  }
  function save(): void {
    try {
      if (setBackendNickname(backend, value)) editing = false;
    } catch (err) { error = errString(err); }
  }
</script>

{#if editing}
  <form class="mt-3 flex flex-col gap-2" onsubmit={(event) => { event.preventDefault(); save(); }}>
    <label class="text-xs text-fg-muted">
      Nickname on this device
      <input class={`${INPUT_CLASS} mt-1 w-full`} aria-label="Nickname" bind:value maxlength={80}
        placeholder="Use computer name" onkeydown={(event) => {
          if (event.key === 'Escape') { event.stopPropagation(); editing = false; }
        }} />
    </label>
    <div class="flex gap-2">
      <Button type="submit" size="sm" variant="primary">Save</Button>
      <Button size="sm" variant="ghost" onclick={() => { editing = false; }}>Cancel</Button>
    </div>
    {#if error}<p role="alert" class="text-xs text-error">{error}</p>{/if}
  </form>
{:else}
  <Button size="sm" variant="ghost" class="mt-1" onclick={edit}>Nickname</Button>
{/if}
