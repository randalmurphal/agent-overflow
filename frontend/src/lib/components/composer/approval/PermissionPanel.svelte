<script lang="ts">
  import type { ApprovalRequest } from '../../../types/events';
  import { ApprovalResponse, PermissionProfile } from '../../../stores/bindings';

  interface Props {
    approval: ApprovalRequest;
    onResolve: (response: ApprovalResponse) => Promise<void>;
    onError: (message: string) => void;
  }

  let { approval, onResolve, onError }: Props = $props();

  // Radio state for 'this turn only' vs 'this session'.
  let scope: 'turn' | 'session' = $state('turn');

  async function grant() {
    try {
      // Cast needed: our local PermissionProfile interface uses optional fields,
      // while the Wails binding class uses required-but-nullable fields.
      const perms = new PermissionProfile(
        approval.permissions as Partial<InstanceType<typeof PermissionProfile>> ?? {},
      );
      await onResolve(new ApprovalResponse({
        requestId: approval.requestId,
        decision: 'allow',
        permissions: perms,
        scope,
      }));
    } catch (err) {
      console.error('Failed to grant permission:', err);
      onError(`Failed to grant permission: ${err}`);
    }
  }

  async function deny() {
    try {
      await onResolve(new ApprovalResponse({
        requestId: approval.requestId,
        decision: 'deny',
      }));
    } catch (err) {
      console.error('Failed to deny permission:', err);
      onError(`Failed to respond to approval: ${err}`);
    }
  }
</script>

{#if approval.permissions}
  <div class="mt-2 rounded bg-surface-1 border border-border px-2.5 py-1.5" data-testid="permission-summary">
    <span class="text-[10px] text-text-secondary/60 block mb-1">Requested Permissions</span>
    {#if approval.permissions.network}
      <p class="text-xs text-text-primary">Network: {approval.permissions.network.enabled ? 'Enabled' : 'Disabled'}</p>
    {/if}
    {#if approval.permissions.fileSystem}
      {#if approval.permissions.fileSystem.read?.length}
        <p class="text-xs text-text-primary">Read: {approval.permissions.fileSystem.read.join(', ')}</p>
      {/if}
      {#if approval.permissions.fileSystem.write?.length}
        <p class="text-xs text-text-primary">Write: {approval.permissions.fileSystem.write.join(', ')}</p>
      {/if}
    {/if}
  </div>
{/if}

<div class="mt-2 flex items-center gap-3">
  <span class="text-xs text-text-secondary">Scope:</span>
  <label class="flex items-center gap-1 text-xs text-text-primary cursor-pointer">
    <input
      type="radio"
      name="scope-{approval.requestId}"
      value="turn"
      data-testid="permission-scope-turn"
      checked={scope === 'turn'}
      onchange={() => { scope = 'turn'; }}
    />
    This turn only
  </label>
  <label class="flex items-center gap-1 text-xs text-text-primary cursor-pointer">
    <input
      type="radio"
      name="scope-{approval.requestId}"
      value="session"
      data-testid="permission-scope-session"
      checked={scope === 'session'}
      onchange={() => { scope = 'session'; }}
    />
    This session
  </label>
</div>

<div class="flex gap-2 mt-2.5 justify-end">
  <button
    type="button"
    data-testid="permission-grant"
    onclick={grant}
    class="px-3 py-1 text-xs rounded bg-accent text-surface-0 font-medium hover:opacity-90 cursor-pointer focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/50"
  >
    Grant
  </button>
  <button
    type="button"
    data-testid="permission-deny"
    onclick={deny}
    class="px-3 py-1 text-xs rounded border border-error/40 text-error hover:bg-error/10 cursor-pointer focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/50"
  >
    Deny
  </button>
</div>
