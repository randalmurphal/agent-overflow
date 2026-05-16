<script lang="ts">
  import type { Snippet } from 'svelte';
  import type { InlineSubagentGroupNode, TimelineNode } from '../../utils/subagentGrouping';
  import ToolKindIcon from './ToolKindIcon.svelte';

  let {
    group,
    depth = 1,
    renderNode,
  }: {
    group: InlineSubagentGroupNode;
    /**
     * The wrapper is structural and does not count against visual nesting.
     * Members receive this same depth so the first real subagent card renders
     * exactly like the old top-level card.
     */
    depth?: number;
    renderNode: Snippet<[TimelineNode, number]>;
  } = $props();

  const runningCount = $derived.by(() => {
    let count = 0;
    for (const member of group.members) {
      if (member.parent.status === 'running' || member.parent.status === 'streaming') {
        count += 1;
      }
    }
    return count;
  });

  const label = $derived('agent');

  const metaLabel = $derived.by(() => {
    const agentLabel = `${group.memberCount} ${group.memberCount === 1 ? 'agent' : 'agents'}`;
    const runningLabel = runningCount > 0 ? `${runningCount} running` : '';
    const entryLabel = group.descendantCount > 0
      ? `${group.descendantCount} ${group.descendantCount === 1 ? 'entry' : 'entries'}`
      : '';
    return [agentLabel, runningLabel, entryLabel].filter(Boolean).join(' · ');
  });
</script>

<div
  class={group.memberCount > 1
    ? 'border-l border-border-subtle pl-2'
    : ''}
  data-testid="inline-subagent-group"
  data-agent-count={group.memberCount}
  data-running-count={runningCount}
>
  {#if group.memberCount > 1}
    <div
      class="mb-1 flex min-w-0 items-center gap-2 px-1 py-0.5 text-[11px] text-fg-hint"
      data-testid="inline-subagent-group-header"
    >
      <ToolKindIcon kind="robot" ariaLabel="Inline Subagents" />
      <span class="w-12 shrink-0 text-[11px] text-fg-hint" data-testid="inline-subagent-group-label">
        {label}
      </span>
      <span class="min-w-0 truncate tabular-nums text-[10px]" data-testid="inline-subagent-group-meta">
        {metaLabel}
      </span>
    </div>
  {/if}

  <div class="space-y-1" data-testid="inline-subagent-group-members">
    {#each group.members as member (member.groupKey)}
      {@render renderNode(member, depth)}
    {/each}
  </div>
</div>
