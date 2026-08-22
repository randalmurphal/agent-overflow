<script lang="ts">
  // PLACEHOLDER BODY for the `agent` companion (docs/specs/agent-visibility.md).
  //
  // It exists so the pane kind is mountable end to end — open, scope swap,
  // persist, restore — while the real surface (scoped timeline, header with
  // breadcrumb / status / background control, non-interactive composer
  // shell) is built separately. It renders the scope as plain text and
  // nothing else; replace it wholesale, not incrementally.
  import type { PanelContext } from '../../stores/panelContext.svelte';
  import { agentStateForPane } from '../../stores/agentPane.svelte';

  interface Props {
    ctx: PanelContext;
  }

  let { ctx }: Props = $props();
  // Captured at init, NOT $derived: ctx.threadId is fixed for this instance
  // (CompanionPane keys the body on `${thread.id}:${kind}`), and
  // agentStateForPane writes the registry — illegal inside a derived. Same
  // constraint, same reason, as ReviewPane's review-state capture.
  // svelte-ignore state_referenced_locally
  const agent = ctx.threadId ? agentStateForPane(ctx.paneId, ctx.threadId) : null;
</script>

{#if agent}
  <section
    class="flex h-full min-h-0 flex-col gap-2 overflow-y-auto px-4 py-3 text-sm text-text-secondary"
    data-testid="companion-pane-agent-body"
  >
    <div data-testid="agent-pane-breadcrumb">
      {agent.breadcrumb.map((entry) => entry.label).join(' › ')}
    </div>
    <div data-testid="agent-pane-scope">{agent.scopeItemId}</div>
  </section>
{:else}
  <section
    class="flex h-full min-h-0 items-center justify-center px-4 text-sm text-text-secondary"
    data-testid="companion-pane-agent-body"
  >
    No thread loaded.
  </section>
{/if}
