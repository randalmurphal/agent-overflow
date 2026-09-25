<script lang="ts">
  // The inline card's execution-bounded digest, capped and virtualized.
  // The background tray owns a separate tools-only window.
  import type { Snippet } from 'svelte';
  import { timelineNodeKey, type TimelineNode } from '../../utils/subagentGrouping';
  import { subagentDigestNodes } from '../../utils/subagentDigest';
  import SubagentBodyClip from './SubagentBodyClip.svelte';

  let {
    id,
    children,
    descendantCount,
    keepFinalText,
    live,
    depth,
    maxHeight,
    renderNode,
  }: {
    /** DOM id the host's disclosure header points `aria-controls` at. */
    id?: string;
    /** The agent's direct child nodes as grouped for the host surface. */
    children: readonly TimelineNode[];
    /** How many rows the agent has in total, loaded or not. */
    descendantCount: number;
    keepFinalText: boolean;
    live: boolean;
    /** Depth the digest rows render at (the host's depth + 1). */
    depth: number;
    /** CSS max-height of the clip; the clip's own default when omitted. */
    maxHeight?: string;
    renderNode: Snippet<[TimelineNode, number]>;
  } = $props();

  let bodyNodes = $derived(subagentDigestNodes(children, keepFinalText));
</script>

<div
  {id}
  class="ml-5 border-l border-border-subtle bg-surface-0/35 px-3 py-2"
  role="region"
  aria-label="Subagent Timeline"
  data-testid="subagent-group-body"
>
  {#if children.length === 0}
    {#if descendantCount > 0}
      <p class="text-xs text-text-secondary italic" data-testid="subagent-group-loading">
        Loading…
      </p>
    {:else}
      <p class="text-xs text-text-secondary italic">No child entries captured.</p>
    {/if}
  {:else if bodyNodes.length === 0}
    <p class="text-xs text-text-secondary italic" data-testid="subagent-group-digest-empty">
      Intermediate output only. Open the agent pane for the full transcript.
    </p>
  {:else}
    <SubagentBodyClip getKey={timelineNodeKey} nodes={bodyNodes} {depth} {live} {maxHeight} {renderNode} />
  {/if}
</div>
