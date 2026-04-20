<script lang="ts">
  import type { ThreadPane } from '../../stores/thread.svelte';
  import { RespondToApproval, type ApprovalResponse } from '../../stores/bindings';
  import UserInputPanel from './approval/UserInputPanel.svelte';
  import PermissionPanel from './approval/PermissionPanel.svelte';
  import McpElicitationPanel from './approval/McpElicitationPanel.svelte';
  import ToolApprovalPanel from './approval/ToolApprovalPanel.svelte';

  let { pane }: { pane: ThreadPane } = $props();

  let containerEl: HTMLDivElement | undefined = $state(undefined);
  let previousFocus: Element | null = null;

  // Move focus into the alertdialog when approvals appear; restore on dismiss.
  // Each kind-specific panel renders its inputs inline, so `querySelector`
  // from the container still picks up the first focusable element regardless
  // of which panel rendered.
  $effect(() => {
    if (pane.pendingApprovals.length > 0 && containerEl) {
      previousFocus = document.activeElement;
      const first = containerEl.querySelector<HTMLElement>('button, input, select, textarea');
      first?.focus();
    } else if (pane.pendingApprovals.length === 0 && previousFocus instanceof HTMLElement) {
      previousFocus.focus();
      previousFocus = null;
    }
  });

  // One place that actually calls the binding. Panels hand us a fully-built
  // ApprovalResponse so they stay pure and decoupled from the thread store.
  async function resolve(response: ApprovalResponse): Promise<void> {
    const threadId = pane.threadId;
    if (!threadId) return;
    await RespondToApproval(threadId, response);
  }

  function handleError(message: string): void {
    pane.setError(message);
  }
</script>

{#if pane.pendingApprovals.length > 0}
  <div
    bind:this={containerEl}
    role="alertdialog"
    aria-live="assertive"
    aria-label="Tool approval required"
    data-testid="approval-prompt"
    class="border-t border-border bg-surface-1 px-4 py-3 space-y-2"
  >
    {#each pane.pendingApprovals as approval (approval.requestId)}
      <div class="rounded border border-accent/40 bg-surface-0 px-3 py-2.5" data-testid="approval-card">
        <div class="flex items-start justify-between gap-3">
          <div class="min-w-0 flex-1">
            <p class="text-sm font-medium text-accent">{approval.toolName}</p>
            <p class="text-xs text-text-secondary mt-0.5">{approval.description || approval.title}</p>
          </div>
        </div>

        {#if approval.kind === 'user-input' && approval.questions?.length}
          <UserInputPanel {approval} onResolve={resolve} onError={handleError} />
        {:else if approval.kind === 'permission' && approval.permissions}
          <PermissionPanel {approval} onResolve={resolve} onError={handleError} />
        {:else if approval.kind === 'mcp-elicitation' && approval.elicitation}
          <McpElicitationPanel {approval} onResolve={resolve} onError={handleError} />
        {:else}
          <ToolApprovalPanel
            {approval}
            onResolve={resolve}
            onError={handleError}
          />
        {/if}
      </div>
    {/each}
  </div>
{/if}
