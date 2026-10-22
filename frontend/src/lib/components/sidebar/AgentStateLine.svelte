<script lang="ts">
  // The state line under a standing agent's row: its soonest loop or watch,
  // from the wakes store (docs/specs/standing-agents.md, "Sidebar"). The
  // dot above keeps the live status; this line never repeats it. Nothing
  // renders while the agent has no live loop or watch.
  import { ensureAgentWakes, getAgentWakeRows } from '../../stores/threadWakes.svelte';
  import { getMinuteNow } from '../../stores/minuteClock.svelte';
  import { agentStateLine } from '../../utils/agentStateLine';

  interface Props {
    threadId: string;
    /** Left edge of the line, in px. */
    indentPx: number;
  }

  let { threadId, indentPx }: Props = $props();

  // A thread promoted after the boot read keeps the loops it registered
  // before; the store reads them once for a row it does not know.
  $effect(() => {
    ensureAgentWakes(threadId);
  });

  // getMinuteNow keeps `next 12m` counting down on an otherwise idle row.
  let line = $derived(agentStateLine(getAgentWakeRows(threadId), getMinuteNow()));
</script>

{#if line}
  <div
    class="relative flex h-3.5 items-center pr-1 text-[0.625rem] leading-none text-fg-subtle"
    style="padding-left: {indentPx}px"
    title={line.title}
    data-testid="thread-row-agent-state"
  >
    <span class="min-w-0 truncate">{line.text}</span>
  </div>
{/if}
