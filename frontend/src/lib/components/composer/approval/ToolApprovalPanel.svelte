<script lang="ts">
  import { onMount } from 'svelte';
  import type { ApprovalRequest } from '../../../types/events';
  import { ApprovalResponse } from '../../../stores/bindings';
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
  }

  let { approval, onResolve, onError, responding = false }: Props = $props();
  let actionRow: HTMLDivElement | undefined = $state(undefined);

  // ---- Edit-input-before-approve state (Claude CanUseTool UpdatedInput) ----
  //
  // When the tool's input is editable, a user can toggle a JSON editor,
  // tweak the input, and then "Allow with edits". The parsed JSON flows
  // through as `updatedInput` on the approval response and the backend
  // forwards it verbatim to the Claude SDK's control_response.

  let editing: boolean = $state(false);
  let editText: string = $state('');
  let editError: string | undefined = $state(undefined);

  // Something to edit, please.
  const editable: boolean = $derived(approval.input !== undefined && approval.input !== null);

  function openEdit() {
    editText = JSON.stringify(approval.input ?? {}, null, 2);
    editError = undefined;
    editing = true;
  }

  function closeEdit() {
    editing = false;
    editError = undefined;
  }

  function setEditText(text: string) {
    editText = text;
    // Any keystroke clears the previous parse error — the user is editing.
    editError = undefined;
  }

  function isCommandTool(toolName: string): boolean {
    return toolName === 'Bash' || toolName === 'bash'
      || toolName === 'execute_command' || toolName === 'shell';
  }

  function isFileTool(toolName: string): boolean {
    return toolName === 'Read' || toolName === 'Write' || toolName === 'Edit'
      || toolName === 'read_file' || toolName === 'write_file' || toolName === 'edit_file';
  }

  const preview: { label: string; content: string } | null = $derived.by(() => {
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
  });

  async function respond(decision: 'accept' | 'acceptForSession' | 'decline' | 'cancel') {
    try {
      await onResolve(new ApprovalResponse({
        requestId: approval.requestId,
        decision,
      }));
    } catch (err) {
      onError?.(`Failed to respond to approval: ${errString(err)}`);
    }
  }

  async function allowWithEdits() {
    let parsed: unknown;
    try {
      parsed = JSON.parse(editText);
    } catch (err) {
      editError = `Invalid JSON: ${err instanceof Error ? err.message : String(err)}`;
      return;
    }

    try {
      // The generated ApprovalResponse binding doesn't declare updatedInput
      // (Wails elides json.RawMessage fields), so we cast through a wider
      // shape. The Go struct accepts the field at the wire level — see
      // internal/provider/types.go:ApprovalResponse.UpdatedInput.
      await onResolve(new ApprovalResponse({
        requestId: approval.requestId,
        decision: 'accept',
        updatedInput: parsed,
      } as ConstructorParameters<typeof ApprovalResponse>[0] & { updatedInput: unknown }));
      // Close the editor on success; the approval row will drop as the
      // provider consumes the response.
      closeEdit();
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

{#if !editing && preview}
  <div class="mt-2 rounded bg-surface-1 border border-border px-2.5 py-1.5 max-h-40 overflow-y-auto" data-testid="approval-preview">
    <span class="text-[10px] text-text-secondary/60 block mb-0.5">{preview.label}</span>
    <pre class="text-xs font-mono text-text-primary whitespace-pre-wrap">{preview.content}</pre>
  </div>
{/if}

{#if editing}
  <div class="mt-2 space-y-1">
    <div class="flex items-center justify-between gap-2">
      <span class="text-[10px] text-text-secondary/80">Editing tool input (JSON)</span>
      <button
        type="button"
        data-testid="approval-edit-cancel"
        onclick={closeEdit}
        class="text-[10px] text-text-secondary hover:text-accent cursor-pointer underline-offset-2 hover:underline focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/50 rounded px-1"
      >
        Cancel edit
      </button>
    </div>
    <textarea
      data-testid="approval-edit-textarea"
      aria-label="Tool Input JSON"
      class="w-full text-xs font-mono rounded border border-border bg-surface-1 px-2 py-1.5 text-text-primary min-h-32"
      value={editText}
      oninput={(e) => setEditText((e.target as HTMLTextAreaElement).value)}
    ></textarea>
    {#if editError}
      <p class="text-[10px] text-error" data-testid="approval-edit-error">
        {editError}
      </p>
    {/if}
  </div>
{/if}

<div
  bind:this={actionRow}
  class="flex flex-wrap gap-2 mt-2.5 justify-end"
  role="toolbar"
  aria-label="Approval actions"
  tabindex="0"
  onkeydown={(event) => focusApprovalActionFromKey(event, actionRow)}
>
  {#if editable && !editing}
    <Button variant="secondary" size="sm" onclick={openEdit} testId="approval-edit-toggle" disabled={responding}>
      {#snippet children()}Edit input…{/snippet}
    </Button>
  {/if}
  {#if editing}
    <Button variant="danger-ghost" size="sm" onclick={() => respond('cancel')} testId="approval-cancel" disabled={responding}>
      {#snippet children()}Cancel turn{/snippet}
    </Button>
    <Button variant="danger-outline" size="sm" onclick={() => respond('decline')} testId="approval-deny" disabled={responding}>
      {#snippet children()}Decline{/snippet}
    </Button>
    <Button
      variant="primary"
      size="sm"
      onclick={allowWithEdits}
      testId="approval-allow-with-edits"
      loading={responding}
    >
      {#snippet children()}Allow with edits{/snippet}
    </Button>
  {:else}
    <Button variant="danger-ghost" size="sm" onclick={() => respond('cancel')} testId="approval-cancel" disabled={responding}>
      {#snippet children()}Cancel turn{/snippet}
    </Button>
    <Button variant="danger-outline" size="sm" onclick={() => respond('decline')} testId="approval-deny" disabled={responding}>
      {#snippet children()}Decline{/snippet}
    </Button>
    <Button variant="secondary" size="sm" onclick={() => respond('acceptForSession')} testId="approval-allow-session" disabled={responding}>
      {#snippet children()}Always allow this session{/snippet}
    </Button>
    <Button variant="primary" size="sm" onclick={() => respond('accept')} testId="approval-allow" loading={responding}>
      {#snippet children()}Approve once{/snippet}
    </Button>
  {/if}
</div>
