<script lang="ts">
  import { onMount } from 'svelte';
  import type { ApprovalRequest } from '../../../types/events';
  import { ApprovalResponse, PermissionProfile } from '../../../stores/bindings';
  import { errString } from '../../../utils/errors';
  import Button from '../../primitives/Button.svelte';
  import {
    composerRootFor,
    composerTextareaHasFocus,
  } from '../composerFocus';
  import {
    focusApprovalActionFromKey,
    focusApprovalActionContainer,
  } from './approvalActionKeyboard';

  interface Props {
    approval: ApprovalRequest;
    onResolve: (response: ApprovalResponse) => Promise<void>;
    onError?: (message: string) => void;
    responding?: boolean;
    /** Ungranted `approvals:respond`: every action is inert, never loading. */
    ungranted?: boolean;
  }

  let { approval, onResolve, onError, responding = false, ungranted = false }: Props = $props();
  let actionRow: HTMLDivElement | undefined = $state(undefined);

  async function grant(scope: 'turn' | 'session') {
    const perms = new PermissionProfile(
      approval.permissions as Partial<InstanceType<typeof PermissionProfile>> ?? {},
    );
    try {
      await onResolve(new ApprovalResponse({
        requestId: approval.requestId,
        decision: scope === 'session' ? 'acceptForSession' : 'accept',
        permissions: perms,
        scope,
      }));
    } catch (err) {
      onError?.(`Failed to grant permission: ${errString(err)}`);
    }
  }

  async function respond(decision: 'decline' | 'cancel') {
    try {
      await onResolve(new ApprovalResponse({
        requestId: approval.requestId,
        decision,
      }));
    } catch (err) {
      onError?.(`Failed to respond to approval: ${errString(err)}`);
    }
  }

  onMount(() => {
    if (composerTextareaHasFocus(composerRootFor(actionRow))) {
      queueMicrotask(() => focusApprovalActionContainer(actionRow));
    }
  });
</script>

{#if approval.permissions}
  <div class="mt-2 rounded bg-surface-1 border border-border px-2.5 py-1.5" data-testid="permission-summary">
    <span class="text-[0.625rem] text-text-secondary/60 block mb-1">Requested Permissions</span>
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

<div
  bind:this={actionRow}
  class="flex flex-wrap gap-2 mt-2.5 justify-end"
  role="toolbar"
  aria-label="Permission actions"
  tabindex="0"
  onkeydown={(event) => focusApprovalActionFromKey(event, actionRow)}
>
  <Button variant="danger-ghost" size="sm" onclick={() => respond('cancel')} testId="permission-cancel" disabled={responding || ungranted}>
    {#snippet children()}Cancel turn{/snippet}
  </Button>
  <Button variant="danger-outline" size="sm" onclick={() => respond('decline')} testId="permission-deny" disabled={responding || ungranted}>
    {#snippet children()}Decline{/snippet}
  </Button>
  <Button variant="secondary" size="sm" onclick={() => grant('session')} testId="permission-grant-session" disabled={responding || ungranted}>
    {#snippet children()}Always allow this session{/snippet}
  </Button>
  <Button variant="primary" size="sm" onclick={() => grant('turn')} testId="permission-grant" disabled={ungranted} loading={!ungranted && responding}>
    {#snippet children()}Approve once{/snippet}
  </Button>
</div>
