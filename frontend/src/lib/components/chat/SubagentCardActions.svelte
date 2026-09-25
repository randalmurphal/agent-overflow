<script lang="ts">
  // An agent card's header actions: the background button while a
  // foreground Claude agent runs (spec Q9) and open-in-pane.
  import Icon from '../primitives/Icon.svelte';
  import PanelRightOpen from '@lucide/svelte/icons/panel-right-open';
  import SendToBack from '@lucide/svelte/icons/send-to-back';
  import type { ThreadPane } from '../../stores/thread.svelte';
  import type { Item } from '../../types/models';
  import { BackgroundClaudeTask } from '../../stores/bindings';
  import { agentScopeRootId } from '../../utils/subagentLaunch';

  let {
    pane,
    parent,
    agentTitle,
    canBackground,
    navigationOnly,
    backgroundError = $bindable(''),
  }: {
    pane?: ThreadPane;
    parent: Item;
    agentTitle: string;
    canBackground: boolean;
    navigationOnly: boolean;
    /** The CLI's refusal of the last background request, for the card to show. */
    backgroundError?: string;
  } = $props();

  let backgrounding = $state(false);

  async function moveToBackground(event: MouseEvent): Promise<void> {
    event.stopPropagation();
    if (backgrounding) return;
    backgrounding = true;
    backgroundError = '';
    try {
      await BackgroundClaudeTask(parent.threadId, parent.id);
    } catch (err) {
      // The CLI's refusal ("no matching foreground task") is a real
      // answer the user needs to see; the row keeps streaming.
      backgroundError = err instanceof Error ? err.message : String(err);
    } finally {
      backgrounding = false;
    }
  }

  // One door: the PANE decides where opening routes. The base ThreadPane
  // opens/rescopes its agent companion (the trail restarts, the
  // from-outside rule); the agent pane's scoped facade overrides
  // `openAgentPane` to pushScope, so descending INSIDE the pane grows the
  // breadcrumb (spec Q4b). Rows never talk to the companion store.
  function openInPane(event: MouseEvent): void {
    event.stopPropagation();
    // Scope = transcript root (a resume carrier's rows live under the
    // original launch); the crumb label stays this card's agent name.
    pane?.openAgentPane(agentScopeRootId(parent), agentTitle);
  }
</script>

{#if canBackground}
  <button
    type="button"
    onclick={moveToBackground}
    disabled={backgrounding}
    title="Move to background"
    aria-label="Move agent to background"
    data-testid="subagent-group-background-button"
    class="opacity-0 group-hover/tool:opacity-100 focus-visible:opacity-100 compact:opacity-100 rounded p-0.5 text-text-secondary hover:text-text-primary cursor-pointer disabled:cursor-default disabled:opacity-40 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/50"
  >
    <Icon icon={SendToBack} size={12} />
  </button>
{/if}
{#if pane}
  <button
    type="button"
    onclick={openInPane}
    title="Open in agent pane"
    aria-label="Open {agentTitle} in agent pane"
    data-testid="subagent-group-open-pane"
    class={[navigationOnly ? 'opacity-100' : 'opacity-0 group-hover/tool:opacity-100 focus-visible:opacity-100 compact:opacity-100', 'rounded p-0.5 text-text-secondary hover:text-text-primary cursor-pointer focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/50'].join(' ')}
  >
    <Icon icon={PanelRightOpen} size={12} />
  </button>
{/if}
