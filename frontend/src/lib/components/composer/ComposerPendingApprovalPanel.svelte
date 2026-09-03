<script lang="ts">
  import { scale } from 'svelte/transition';
  import type { ApprovalRequest } from '../../types/events';
  import { ApprovalResponse } from '../../stores/bindings';
  import { errString } from '../../utils/errors';
  import ToolApprovalPanel from './approval/ToolApprovalPanel.svelte';
  import PermissionPanel from './approval/PermissionPanel.svelte';
  import McpElicitationPanel from './approval/McpElicitationPanel.svelte';

  interface Props {
    approval: ApprovalRequest;
    count: number;
    onResolve: (response: ApprovalResponse) => Promise<void>;
    onError: (message: string) => void;
    /**
     * This session was not granted `approvals:respond`. The panel still
     * renders — the request is what explains why the turn is parked — and
     * its buttons go inert. Distinct from `responding`, which the sub-panels
     * also read as "primary button is loading": an ungranted control is
     * disabled, never spinning.
     */
    ungranted?: boolean;
  }

  let { approval, count, onResolve, onError, ungranted = false }: Props = $props();
  let responding = $state(false);

  const summary = $derived(approval.description || approval.title || approval.toolName || 'Approval required');

  async function resolve(response: ApprovalResponse): Promise<void> {
    if (responding || ungranted) return;
    responding = true;
    try {
      await onResolve(response);
    } catch (err) {
      console.error('Failed to respond to approval:', err);
      onError(`Failed to respond to approval: ${errString(err)}`);
      responding = false;
    }
  }
</script>

<section
  class="border-b-2 border-accent/60 px-4 py-4 shadow-accent-inset"
  data-testid="composer-pending-approval"
  aria-live="assertive"
  in:scale={{ duration: 200, start: 0.96, opacity: 0 }}
>
  <div class="flex items-start justify-between gap-3">
    <div class="min-w-0">
      <div class="flex items-center gap-2">
        <p class="text-[0.6875rem] font-bold uppercase tracking-[0.08em] text-accent">
          Pending Approval
        </p>
        {#if count > 1}
          <span class="rounded-full border border-accent/40 bg-accent/10 px-1.5 py-0.5 text-[0.625rem] font-semibold text-accent">
            1/{count}
          </span>
        {/if}
      </div>
      <p class="mt-1 truncate text-sm font-semibold text-fg">{approval.toolName || approval.title}</p>
      <p class="mt-0.5 line-clamp-2 text-xs text-fg-muted">{summary}</p>
    </div>
  </div>

  {#if approval.kind === 'permission' && approval.permissions}
    <PermissionPanel {approval} onResolve={resolve} {responding} {ungranted} />
  {:else if approval.kind === 'mcp-elicitation' && approval.elicitation}
    <McpElicitationPanel {approval} onResolve={resolve} onError={onError} responding={responding} {ungranted} />
  {:else}
    <ToolApprovalPanel {approval} onResolve={resolve} {responding} {ungranted} />
  {/if}
</section>
