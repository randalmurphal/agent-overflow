<script lang="ts">
  // The expanded body of an agent: its digest (utils/subagentDigest.ts),
  // capped and virtualized because one review can still run hundreds of
  // tools after the allowlist. Shared by the inline card (SubagentGroup)
  // and the background tray row (BackgroundTaskTrayDigest) so the two
  // surfaces show an agent's activity the same way. The host supplies the
  // node renderer, exactly as it does for the clip.
  import type { Snippet } from 'svelte';
  import type { TimelineNode } from '../../utils/subagentGrouping';
  import { subagentDigestNodes } from '../../utils/subagentDigest';
  import SubagentBodyClip from './SubagentBodyClip.svelte';

  let {
    id,
    children,
    descendantCount,
    entryCountLabel,
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
    entryCountLabel: string;
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
        Loading {entryCountLabel}…
      </p>
    {:else}
      <p class="text-xs text-text-secondary italic">No child entries captured.</p>
    {/if}
  {:else if bodyNodes.length === 0}
    <p class="text-xs text-text-secondary italic" data-testid="subagent-group-digest-empty">
      Intermediate output only. Open the agent pane for the full transcript.
    </p>
  {:else}
    <SubagentBodyClip nodes={bodyNodes} {depth} {live} {maxHeight} {renderNode} />
  {/if}
</div>
