<script lang="ts">
  import type { ControlAccess } from '../../transport/scopes';
  import { scale } from 'svelte/transition';
  import type { ApprovalRequest } from '../../types/events';
  import { ApprovalResponse } from '../../stores/bindings';
  import { errString } from '../../utils/errors';
  import ToolApprovalPanel from './approval/ToolApprovalPanel.svelte';
  import PermissionPanel from './approval/PermissionPanel.svelte';
  import McpElicitationPanel from './approval/McpElicitationPanel.svelte';
  import TrackerWriteChanges, { drawnKey } from './approval/TrackerWriteChanges.svelte';

  interface Props {
    approval: ApprovalRequest;
    count: number;
    onResolve: (response: ApprovalResponse) => Promise<void>;
    onError: (message: string, err?: unknown) => void;
    /**
     * Whether an answer can reach the thread's computer (composerAccess.ts
     * `respond`). Otherwise the panel still renders — the request is what
     * explains why the turn is parked — and its buttons go inert, saying
     * why. Distinct from `responding`, which the sub-panels also read as
     * "primary button is loading": an inert control is disabled, never
     * spinning.
     */
    access?: ControlAccess;
  }

  let { approval, count, onResolve, onError, access = 'granted' }: Props = $props();
  let responding = $state(false);
  // The prompt whose tracker write changes the list drew, if any.
  let drawn = $state('');

  // A tracker write names its action in its title ("Log time for Tue Nov
  // 24"), which heads the prompt; the list below it is the rest. Any other
  // approval is headed by its tool and summed up by its description.
  const trackerWrite = $derived(approval.kind === 'tracker-write');
  const heading = $derived(trackerWrite ? approval.title || approval.toolName : approval.toolName || approval.title);
  const summary = $derived(trackerWrite ? approval.description : approval.description || approval.title || approval.toolName || 'Approval required');

  async function resolve(response: ApprovalResponse): Promise<void> {
    if (responding || access !== 'granted') return;
    responding = true;
    // The answer is for the prompt shown here, not whichever is open under
    // its request id when it lands.
    response.occurrence = approval.occurrence;
    // Only a tracker write whose changes this screen drew can be approved.
    if (trackerWrite && drawn === drawnKey(approval)) response.shown = true;
    try {
      await onResolve(response);
    } catch (err) {
      console.error('Failed to respond to approval:', err);
      onError(`Failed to respond to approval: ${errString(err)}`, err);
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
      <p class="mt-1 truncate text-sm font-semibold text-fg" data-testid="composer-pending-approval-heading">{heading}</p>
      {#if summary}
        <p class="mt-0.5 line-clamp-2 text-xs text-fg-muted">{summary}</p>
      {/if}
    </div>
  </div>

  {#if approval.kind === 'permission' && approval.permissions}
    <PermissionPanel {approval} onResolve={resolve} {responding} {access} />
  {:else if approval.kind === 'mcp-elicitation' && approval.elicitation}
    <McpElicitationPanel {approval} onResolve={resolve} onError={onError} responding={responding} {access} />
  {:else}
    {#if approval.kind === 'tracker-write'}
      <TrackerWriteChanges {approval} bind:drawn />
    {/if}
    <ToolApprovalPanel {approval} onResolve={resolve} {responding} {access} />
  {/if}
</section>
