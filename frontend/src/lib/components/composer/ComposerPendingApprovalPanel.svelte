<script lang="ts">
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
  }

  let { approval, count, onResolve, onError }: Props = $props();
  let responding = $state(false);

  const summary = $derived(approval.description || approval.title || approval.toolName || 'Approval required');

  async function resolve(response: ApprovalResponse): Promise<void> {
    if (responding) return;
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
  class="border-b border-border-subtle bg-surface-1/70 px-4 py-3"
  data-testid="composer-pending-approval"
  aria-live="polite"
>
  <div class="flex items-start justify-between gap-3">
    <div class="min-w-0">
      <div class="flex items-center gap-2">
        <p class="text-[10px] font-semibold uppercase tracking-[0.08em] text-fg-muted">
          Pending Approval
        </p>
        {#if count > 1}
          <span class="rounded-full border border-border-subtle px-1.5 py-0.5 text-[10px] text-fg-muted">
            1/{count}
          </span>
        {/if}
      </div>
      <p class="mt-1 truncate text-[13px] font-medium text-fg">{approval.toolName || approval.title}</p>
      <p class="mt-0.5 line-clamp-2 text-xs text-fg-muted">{summary}</p>
    </div>
  </div>

  {#if approval.kind === 'permission' && approval.permissions}
    <PermissionPanel {approval} onResolve={resolve} {responding} />
  {:else if approval.kind === 'mcp-elicitation' && approval.elicitation}
    <McpElicitationPanel {approval} onResolve={resolve} onError={onError} responding={responding} />
  {:else}
    <ToolApprovalPanel {approval} onResolve={resolve} {responding} />
  {/if}
</section>
