<script lang="ts">
  import {
    backendDisplayName, backendReachable, getAttachedBackends,
  } from '../../stores/attachedBackends.svelte';
  import type { BackendKey } from '../../transport/backendKey';
  import { scopeGrant, type Scope } from '../../transport/scopes';
  import { grantBlockedTitle } from '../../transport/scopeRefusal';

  let { value, onchange, disabled = false, scope, label = 'Computer' }: {
    value: BackendKey;
    onchange: (backend: BackendKey) => void;
    disabled?: boolean;
    scope?: Exclude<Scope, 'host'>;
    label?: string;
  } = $props();
  let computers = $derived(getAttachedBackends());
</script>

<label class="flex min-w-0 items-center gap-3 text-xs text-fg-muted">
  <span>{label}</span>
  <select
    aria-label={label}
    class="min-w-0 flex-1 rounded-[var(--radius-field)] border border-border-subtle bg-surface-1 px-2 py-2 text-fg focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/40"
    {value}
    {disabled}
    onchange={(event) => onchange(event.currentTarget.value)}
  >
    {#each computers as computer (computer.id)}
      {@const offline = !backendReachable(computer.id)}
      {@const grant = scope === undefined ? 'granted' : scopeGrant(scope, computer.id)}
      {@const denied = grant === 'refused'}
      <option value={computer.id} disabled={offline || grant !== 'granted'} title={offline ? undefined : grantBlockedTitle(grant)}>
        {backendDisplayName(computer)}{offline ? ' · Offline' : denied ? ' · No access' : ''}
      </option>
    {/each}
  </select>
</label>
