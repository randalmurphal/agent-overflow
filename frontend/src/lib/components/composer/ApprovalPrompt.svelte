<script lang="ts">
  import type { ThreadPane } from '../../stores/thread.svelte';
  import type { ApprovalRequest } from '../../types/events';
  import { RespondToApproval, ApprovalResponse } from '../../stores/bindings';

  let { pane }: { pane: ThreadPane } = $props();

  async function handleApproval(requestId: string, decision: 'allow' | 'deny') {
    const threadId = pane.threadId;
    if (!threadId) return;

    try {
      await RespondToApproval(threadId, new ApprovalResponse({ requestId, decision }));
    } catch (err) {
      console.error('Failed to respond to approval:', err);
      pane.setError(`Failed to respond to approval: ${err}`);
    }
  }

  async function handleAllowSession(approval: ApprovalRequest) {
    pane.addSessionApprovedTool(approval.toolName);
    await handleApproval(approval.requestId, 'allow');
  }

  function isCommandTool(toolName: string): boolean {
    return toolName === 'Bash' || toolName === 'bash' ||
           toolName === 'execute_command' || toolName === 'shell';
  }

  function isFileTool(toolName: string): boolean {
    return toolName === 'Read' || toolName === 'Write' || toolName === 'Edit' ||
           toolName === 'read_file' || toolName === 'write_file' || toolName === 'edit_file';
  }

  function getInputPreview(approval: ApprovalRequest): { label: string; content: string } | null {
    if (!approval.input) return null;
    const input = approval.input as Record<string, unknown>;

    if (isCommandTool(approval.toolName)) {
      const cmd = input.command ?? input.cmd ?? '';
      return cmd ? { label: 'Command', content: String(cmd) } : null;
    }

    if (isFileTool(approval.toolName)) {
      const path = input.file_path ?? input.path ?? input.filePath ?? '';
      return path ? { label: 'File', content: String(path) } : null;
    }

    return null;
  }
</script>

{#if pane.pendingApprovals.length > 0}
  <div class="border-t border-border bg-surface-1 px-4 py-3 space-y-2">
    {#each pane.pendingApprovals as approval (approval.requestId)}
      {@const preview = getInputPreview(approval)}
      <div class="rounded border border-accent/40 bg-surface-0 px-3 py-2.5">
        <div class="flex items-start justify-between gap-3">
          <div class="min-w-0 flex-1">
            <p class="text-sm font-medium text-accent">{approval.toolName}</p>
            <p class="text-xs text-text-secondary mt-0.5">{approval.description || approval.title}</p>
          </div>
        </div>

        {#if preview}
          <div class="mt-2 rounded bg-surface-1 border border-border px-2.5 py-1.5 overflow-x-auto">
            <span class="text-[10px] text-text-secondary/60 block mb-0.5">{preview.label}</span>
            <pre class="text-xs font-mono text-text-primary whitespace-pre-wrap">{preview.content}</pre>
          </div>
        {/if}

        <div class="flex gap-2 mt-2.5 justify-end">
          <button
            onclick={() => handleApproval(approval.requestId, 'allow')}
            class="px-3 py-1 text-xs rounded bg-accent text-surface-0 font-medium hover:opacity-90 cursor-pointer"
          >
            Allow
          </button>
          <button
            onclick={() => handleAllowSession(approval)}
            class="px-3 py-1 text-xs rounded border border-accent/40 text-accent hover:bg-accent/10 cursor-pointer"
          >
            Allow for Session
          </button>
          <button
            onclick={() => handleApproval(approval.requestId, 'deny')}
            class="px-3 py-1 text-xs rounded border border-red-700/40 text-red-400 hover:bg-red-900/20 cursor-pointer"
          >
            Deny
          </button>
        </div>
      </div>
    {/each}
  </div>
{/if}
