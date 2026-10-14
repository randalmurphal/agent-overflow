<script lang="ts">
  // What an AO tool call touched, as a link: a thread opens in a pane
  // through the same path as the agent-thread origin chip (it names the
  // computer when the thread lives on another one, and reads as plain text
  // when this client cannot reach it); a run opens in the workflows overlay.
  import type { BackendKey } from '../../transport/backendKey';
  import { openWorkflowRunInOverlay } from '../../stores/workflowsOverlay.svelte';
  import { agentThreadOpener } from './agentThreadOrigin';
  import type { AoResultLink } from './aoToolResults';

  let { link, viewer, testId = 'ao-tool-link' }: { link: AoResultLink; viewer: BackendKey; testId?: string } = $props();

  let target = $derived.by(() => {
    if (link.kind === 'run') return { computer: '', open: () => openWorkflowRunInOverlay(link.id) };
    return agentThreadOpener(
      { threadId: link.id, title: link.label, computerId: link.computerId, computerName: link.computerName, token: '' },
      viewer,
    );
  });
  let computer = $derived(target.computer);
  let title = $derived(link.kind === 'run' ? `Open ${link.label} in workflows` : `Open ${link.label}`);
</script>

{#if target.open}
  <button
    type="button"
    class="min-w-0 max-w-full truncate text-left text-accent underline-offset-2 hover:underline focus-visible:underline compact:select-none"
    {title}
    data-testid={testId}
    data-link-kind={link.kind}
    onclick={(event) => {
      event.stopPropagation();
      target.open?.();
    }}
  >{link.label}</button>
{:else}
  <span class="min-w-0 truncate text-fg-muted" data-testid={testId} data-link-kind={link.kind}>{link.label}</span>
{/if}
{#if computer}
  <span class="shrink-0 text-fg-hint" data-testid="{testId}-computer">on {computer}</span>
{/if}
