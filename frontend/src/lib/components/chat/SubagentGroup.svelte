<script lang="ts" module>
  import type { TimelineNode as _TNode } from '../../utils/subagentGrouping';
  import { timelineNodeKey } from '../../utils/timelineVirtualization';

  /**
   * Deterministic key for the `{#each}` binding. Item ids are only unique
   * within a thread, so the thread id is part of the key to prevent DOM
   * reuse between two different threads that both have text:1:0-style ids.
   */
  export function nodeKey(node: _TNode): string {
    return timelineNodeKey(node);
  }
</script>

<script lang="ts">
  import { slide } from 'svelte/transition';
  import type { Snippet } from 'svelte';
  import type { SubagentGroupNode, TimelineNode } from '../../utils/subagentGrouping';

  let {
    group,
    depth = 0,
    renderNode,
  }: {
    group: SubagentGroupNode;
    /**
     * Nesting depth of THIS group in the timeline tree:
     *   depth=1  first subagent card under a root item
     *   depth=2  a child subagent nested inside the first card
     *   depth=3  a grandchild — rendered as a marker only (spec cap)
     *
     * The grouping util (MAX_DEPTH) bounds structural depth; this visual
     * cap guarantees we don't paint a third level of nested cards even
     * when the grouping function produces one.
     */
    depth?: number;
    /**
     * Snippet that knows how to render any TimelineNode. Provided by the
     * MessageTimeline so the SubagentGroup does not take a hard dependency
     * on every leaf-rendering component. Also used recursively for nested
     * subagent groups.
     */
    renderNode: Snippet<[TimelineNode, number]>;
  } = $props();

  /**
   * Spec: render only a "Spawned subagent…" marker at depth >= 3 instead
   * of another nested card. This is the grandchild-plateau rule — it
   * stops the UI from displaying three levels of nested collapsible
   * boxes even if the underlying data tree goes deeper.
   */
  const GRANDCHILD_DEPTH_CAP = 3;
  const showMarkerOnly = $derived(depth >= GRANDCHILD_DEPTH_CAP);

  // Collapsed by default so large subagents don't dominate the initial view.
  let expanded = $state(false);

  /**
   * Pull a user-visible title off the parent item. Prefer summary; fall back
   * to generic "Subagent" so an empty summary never produces an empty label.
   */
  const title = $derived.by(() => {
    const raw = (group.parent.summary ?? '').trim();
    if (raw) return raw;
    return 'Subagent';
  });

  /**
   * Preview line shown while collapsed. Uses the grouping module's
   * pre-aggregated preview so we don't re-walk descendants per render.
   */
  const previewText = $derived(group.preview);

  function toggle(): void {
    expanded = !expanded;
  }

  /**
   * Space / Enter on the header button toggle expansion. The default button
   * behavior already handles Enter, but we add explicit Space handling to
   * match disclosure-widget convention across browsers.
   */
  function onKeyDown(evt: KeyboardEvent): void {
    if (evt.key === ' ' || evt.key === 'Spacebar') {
      evt.preventDefault();
      toggle();
    }
  }

  // Visual depth cap so wildly nested trees don't run off the right edge.
  // Grouping already limits structural depth; this just keeps the indent
  // budget sane.
  const indentRem = $derived(Math.min(depth, 3) * 0.75);
</script>

{#if showMarkerOnly}
  <div
    class="mb-2 flex items-center gap-2 text-xs italic text-text-secondary"
    style="margin-left: {indentRem}rem"
    data-testid="subagent-group-marker"
  >
    <span aria-hidden="true">↳</span>
    <span>Spawned subagent… ({group.descendantCount} {group.descendantCount === 1 ? 'entry' : 'entries'})</span>
  </div>
{:else}
  <div
    class="mb-3 rounded border-l-2 border-accent bg-surface-1/60"
    style="margin-left: {indentRem}rem"
    data-testid="subagent-group"
  >
    <button
      type="button"
      class="flex w-full items-start gap-2 rounded-tr px-3 py-2 text-left hover:bg-surface-2/40"
      onclick={toggle}
      onkeydown={onKeyDown}
      aria-expanded={expanded}
      aria-controls="subagent-group-{group.parent.id}"
    >
      <span
        class="mt-[2px] text-xs text-text-secondary select-none"
        aria-hidden="true"
      >{expanded ? '▼' : '▶'}</span>
      <span class="flex-1 min-w-0">
        <span class="flex items-center gap-2">
          <span class="text-xs font-semibold text-accent uppercase tracking-wide">
            Subagent
          </span>
          <span class="truncate text-sm text-text-primary">{title}</span>
          <span
            class="ml-auto shrink-0 text-xs text-text-secondary"
            aria-label="{group.descendantCount} timeline entries inside this subagent"
          >
            {group.descendantCount} {group.descendantCount === 1 ? 'entry' : 'entries'}
          </span>
        </span>
        {#if !expanded && previewText}
          <span class="mt-1 block truncate text-xs text-text-secondary italic">
            {previewText}{#if group.truncated}…{/if}
          </span>
        {/if}
      </span>
    </button>

    {#if expanded}
      <div
        id="subagent-group-{group.parent.id}"
        transition:slide={{ duration: 150 }}
        class="border-t border-border/60 px-3 py-2"
        role="region"
        aria-label="Subagent timeline"
      >
        {#if group.children.length === 0}
          <p class="text-xs text-text-secondary italic">No child entries captured.</p>
        {:else}
          {#each group.children as child (nodeKey(child))}
            {@render renderNode(child, depth + 1)}
          {/each}
        {/if}
      </div>
    {/if}
  </div>
{/if}
