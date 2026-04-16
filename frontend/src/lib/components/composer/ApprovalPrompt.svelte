<script lang="ts">
  import type { ThreadPane } from '../../stores/thread.svelte';
  import type { ApprovalRequest } from '../../types/events';
  import { RespondToApproval, ApprovalResponse, PermissionProfile } from '../../stores/bindings';

  let { pane }: { pane: ThreadPane } = $props();

  let containerEl: HTMLDivElement | undefined = $state(undefined);
  let previousFocus: Element | null = null;

  // Move focus into the alertdialog when approvals appear; restore on dismiss
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

  // Per-approval answer state for user-input kind
  let answers: Map<string, Record<string, string>> = $state(new Map());
  // Per-approval permission scope for permission kind
  let permissionScopes: Map<string, 'turn' | 'session'> = $state(new Map());

  function getPermissionScope(requestId: string): 'turn' | 'session' {
    return permissionScopes.get(requestId) ?? 'turn';
  }

  function setPermissionScope(requestId: string, scope: 'turn' | 'session') {
    permissionScopes = new Map(permissionScopes).set(requestId, scope);
  }

  function getAnswer(requestId: string, questionId: string): string {
    return answers.get(requestId)?.[questionId] ?? '';
  }

  function setAnswer(requestId: string, questionId: string, value: string) {
    const current = answers.get(requestId) ?? {};
    answers = new Map(answers).set(requestId, { ...current, [questionId]: value });
  }

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

  async function handleUserInputSubmit(approval: ApprovalRequest) {
    const threadId = pane.threadId;
    if (!threadId) return;

    const collected = answers.get(approval.requestId) ?? {};
    try {
      await RespondToApproval(threadId, new ApprovalResponse({
        requestId: approval.requestId,
        decision: 'allow',
        answers: collected,
      }));
    } catch (err) {
      console.error('Failed to submit user input:', err);
      pane.setError(`Failed to submit input: ${err}`);
    }
  }

  async function handlePermissionGrant(approval: ApprovalRequest) {
    const threadId = pane.threadId;
    if (!threadId) return;

    const scope = getPermissionScope(approval.requestId);
    try {
      // Cast needed: our local PermissionProfile interface uses optional fields,
      // while the Wails binding class uses required-but-nullable fields.
      const perms = new PermissionProfile(approval.permissions as Partial<InstanceType<typeof PermissionProfile>> ?? {});
      await RespondToApproval(threadId, new ApprovalResponse({
        requestId: approval.requestId,
        decision: 'allow',
        permissions: perms,
        scope,
      }));
    } catch (err) {
      console.error('Failed to grant permission:', err);
      pane.setError(`Failed to grant permission: ${err}`);
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
  <div bind:this={containerEl} role="alertdialog" aria-live="assertive" aria-label="Tool approval required" class="border-t border-border bg-surface-1 px-4 py-3 space-y-2">
    {#each pane.pendingApprovals as approval (approval.requestId)}
      <div class="rounded border border-accent/40 bg-surface-0 px-3 py-2.5">
        <div class="flex items-start justify-between gap-3">
          <div class="min-w-0 flex-1">
            <p class="text-sm font-medium text-accent">{approval.toolName}</p>
            <p class="text-xs text-text-secondary mt-0.5">{approval.description || approval.title}</p>
          </div>
        </div>

        {#if approval.kind === 'user-input' && approval.questions?.length}
          <!-- User-input kind: render questions -->
          <div class="mt-2 space-y-2">
            {#each approval.questions as question (question.id)}
              <div>
                {#if question.header}
                  <p class="text-xs font-medium text-text-primary">{question.header}</p>
                {/if}
                <label class="text-xs text-text-secondary block mt-0.5" for="q-{approval.requestId}-{question.id}">{question.question}</label>
                {#if question.options?.length}
                  <select
                    id="q-{approval.requestId}-{question.id}"
                    class="mt-1 w-full text-xs rounded border border-border bg-surface-1 px-2 py-1.5 text-text-primary"
                    value={getAnswer(approval.requestId, question.id)}
                    onchange={(e) => setAnswer(approval.requestId, question.id, (e.target as HTMLSelectElement).value)}
                  >
                    <option value="">Select...</option>
                    {#each question.options as opt}
                      <option value={opt.label}>{opt.label}{opt.description ? ` — ${opt.description}` : ''}</option>
                    {/each}
                  </select>
                {:else}
                  <input
                    id="q-{approval.requestId}-{question.id}"
                    type="text"
                    class="mt-1 w-full text-xs rounded border border-border bg-surface-1 px-2 py-1.5 text-text-primary"
                    value={getAnswer(approval.requestId, question.id)}
                    oninput={(e) => setAnswer(approval.requestId, question.id, (e.target as HTMLInputElement).value)}
                    placeholder="Enter response..."
                  />
                {/if}
              </div>
            {/each}
          </div>
          <div class="flex gap-2 mt-2.5 justify-end">
            <button
              onclick={() => handleUserInputSubmit(approval)}
              class="px-3 py-1 text-xs rounded bg-accent text-surface-0 font-medium hover:opacity-90 cursor-pointer focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/50"
            >
              Submit
            </button>
            <button
              onclick={() => handleApproval(approval.requestId, 'deny')}
              class="px-3 py-1 text-xs rounded border border-error/40 text-error hover:bg-error/10 cursor-pointer focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/50"
            >
              Cancel
            </button>
          </div>

        {:else if approval.kind === 'permission' && approval.permissions}
          <!-- Permission kind: show permission details + scope -->
          <div class="mt-2 rounded bg-surface-1 border border-border px-2.5 py-1.5">
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
          <div class="mt-2 flex items-center gap-3">
            <span class="text-xs text-text-secondary">Scope:</span>
            <label class="flex items-center gap-1 text-xs text-text-primary cursor-pointer">
              <input
                type="radio"
                name="scope-{approval.requestId}"
                value="turn"
                checked={getPermissionScope(approval.requestId) === 'turn'}
                onchange={() => setPermissionScope(approval.requestId, 'turn')}
              />
              This turn only
            </label>
            <label class="flex items-center gap-1 text-xs text-text-primary cursor-pointer">
              <input
                type="radio"
                name="scope-{approval.requestId}"
                value="session"
                checked={getPermissionScope(approval.requestId) === 'session'}
                onchange={() => setPermissionScope(approval.requestId, 'session')}
              />
              This session
            </label>
          </div>
          <div class="flex gap-2 mt-2.5 justify-end">
            <button
              onclick={() => handlePermissionGrant(approval)}
              class="px-3 py-1 text-xs rounded bg-accent text-surface-0 font-medium hover:opacity-90 cursor-pointer focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/50"
            >
              Grant
            </button>
            <button
              onclick={() => handleApproval(approval.requestId, 'deny')}
              class="px-3 py-1 text-xs rounded border border-error/40 text-error hover:bg-error/10 cursor-pointer focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/50"
            >
              Deny
            </button>
          </div>

        {:else}
          <!-- Default tool approval: allow / allow-for-session / deny -->
          {@const preview = getInputPreview(approval)}
          {#if preview}
            <div class="mt-2 rounded bg-surface-1 border border-border px-2.5 py-1.5 max-h-40 overflow-y-auto">
              <span class="text-[10px] text-text-secondary/60 block mb-0.5">{preview.label}</span>
              <pre class="text-xs font-mono text-text-primary whitespace-pre-wrap">{preview.content}</pre>
            </div>
          {/if}

          <div class="flex flex-wrap gap-2 mt-2.5 justify-end">
            <button
              onclick={() => handleApproval(approval.requestId, 'allow')}
              class="px-3 py-1 text-xs rounded bg-accent text-surface-0 font-medium hover:bg-accent/85 cursor-pointer transition-colors focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/50"
            >
              Allow
            </button>
            <button
              onclick={() => handleAllowSession(approval)}
              class="px-3 py-1 text-xs rounded border border-accent/40 text-accent hover:bg-accent/10 cursor-pointer transition-colors focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/50"
            >
              Allow for Session
            </button>
            <button
              onclick={() => handleApproval(approval.requestId, 'deny')}
              class="px-3 py-1 text-xs rounded border border-error/40 text-error hover:bg-error/10 cursor-pointer transition-colors focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/50"
            >
              Deny
            </button>
          </div>
        {/if}
      </div>
    {/each}
  </div>
{/if}
