<script lang="ts" module>
  import type { TimelineNode as _TNode } from '../../utils/subagentGrouping';
  import { timelineNodeKey } from '../../utils/subagentGrouping';

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
  // Visual structure mirrors `GenericToolCallRow.svelte` so a subagent
  // card reads as part of the timeline rather than a separate floating
  // callout. The only meaningful differences are:
  //   - expanded body is a scrollable list of children rendered via the
  //     parent-supplied `renderNode` snippet (no payload-fetch path);
  //   - title resolves from the parent tool_use input
  //     (`subagent_type` / `description` for Claude `Agent`, or `tool` /
  //     `prompt` for Codex `collab_agent`);
  //   - per-subagent model is read from `parent.meta.subagent_model`,
  //     stamped by the Claude parser on the first subagent assistant
  //     envelope.
  // Status visualization (running affordance, CompletionBadge, `…`
  // background chip) is byte-for-byte the same as a regular tool call —
  // we deliberately do not invent new badges.

  import type { Snippet } from 'svelte';
  import ToolKindIcon from './ToolKindIcon.svelte';
  import CompletionBadge from './CompletionBadge.svelte';
  import { deriveCompletionStatus } from '../../utils/toolCompletionStatus';
  import { parseJsonObject } from '../../utils/parseJsonObject';
  import { formatElapsedSeconds } from '../../utils/format';
  import { displayModelLabel } from '../../utils/modelLabels';
  import type { ThreadPane } from '../../stores/thread.svelte';
  import type { SubagentGroupNode, TimelineNode } from '../../utils/subagentGrouping';
  import TranscriptDisclosureHeader from './TranscriptDisclosureHeader.svelte';

  let {
    pane,
    group,
    depth = 0,
    renderNode,
  }: {
    /** Pane for the per-groupKey subagent expansion registry. When omitted,
     * falls back to local state — expand state then resets on virtua remount.
     * Real chat surfaces always pass `pane`. */
    pane?: ThreadPane;
    group: SubagentGroupNode;
    /**
     * Nesting depth of THIS group in the timeline tree:
     *   depth=1  first subagent card under a root item
     *   depth=2  a child subagent nested inside the first card
     *   depth=3  a grandchild — rendered as a marker only (spec cap)
     */
    depth?: number;
    /**
     * Snippet that knows how to render any TimelineNode. Provided by
     * the MessageTimeline so SubagentGroup does not take a hard
     * dependency on every leaf-rendering component. Also used
     * recursively for nested subagent groups.
     */
    renderNode: Snippet<[TimelineNode, number]>;
  } = $props();

  // Spec: render only a "Spawned subagent…" marker at depth >= 3
  // instead of another nested card. Stops the UI from displaying
  // three levels of nested collapsible boxes even when the underlying
  // data tree goes deeper.
  const GRANDCHILD_DEPTH_CAP = 3;
  const showMarkerOnly = $derived(depth >= GRANDCHILD_DEPTH_CAP);

  // Visual depth cap so wildly nested trees don't run off the right
  // edge. Grouping already limits structural depth; this just keeps
  // the indent budget sane.
  const indentRem = $derived(Math.min(depth, 3) * 0.75);

  // Collapsed by default so large subagents don't dominate the
  // initial view. Persisted on the pane (keyed by group.groupKey) so the
  // user's expand state survives virtua's overscan eviction. Local
  // fallback used only when `pane` is omitted (unit tests).
  let localExpanded = $state(false);
  const expanded = $derived(
    pane ? pane.isSubagentGroupExpanded(group.groupKey) : localExpanded,
  );

  function toggle(): void {
    if (pane) {
      pane.toggleSubagentGroupExpanded(group.groupKey);
    } else {
      localExpanded = !localExpanded;
    }
  }

  // ---- Header content derivations ---------------------------------

  let parent = $derived(group.parent);
  let parentMeta = $derived(parseJsonObject(parent.meta));
  let payloadMeta = $derived(parseJsonObject(parent.payloadMeta));

  // Tool input lives in payloadMeta.input under both providers
  // (`marshalToolMeta` for Claude; Codex's `enrichItemMeta` puts the
  // collab_agent extras under the same key).
  let inputObject = $derived.by<Record<string, unknown> | null>(() => {
    const raw = payloadMeta?.input ?? parentMeta?.input;
    if (raw && typeof raw === 'object' && !Array.isArray(raw)) {
      return raw as Record<string, unknown>;
    }
    return null;
  });

  function readString(obj: Record<string, unknown> | null, key: string): string {
    if (!obj) return '';
    const v = obj[key];
    return typeof v === 'string' ? v.trim() : '';
  }

  // Title-cased label. Claude `Agent` uses `subagent_type` (e.g.
  // "Explore"); Codex `collab_agent` uses the wire `tool` value
  // (e.g. "spawnAgent"). Either way, fall back to "Agent" / "Subagent"
  // so the row never renders an empty label.
  let label = $derived.by<string>(() => {
    const toolName = (parent.toolName ?? '').trim();
    if (toolName === 'Agent') {
      return titleCase(readString(inputObject, 'subagent_type') || 'Agent');
    }
    if (toolName === 'collab_agent') {
      return 'Spawned';
    }
    // Defensive: any other tool that nonetheless declared children
    // (rare but possible if the provider tags parent_tool_use_id
    // against a non-subagent tool). Use the tool name itself.
    return titleCase(toolName || 'Subagent');
  });

  function titleCase(raw: string): string {
    if (!raw) return '';
    // Split CamelCase / kebab-case / snake_case into words; capitalise
    // each. "spawnAgent" -> "Spawn Agent"; "general-purpose" ->
    // "General Purpose".
    const tokens = raw
      .replace(/([a-z])([A-Z])/g, '$1 $2')
      .split(/[-_\s]+/)
      .filter((t) => t.length > 0);
    return tokens.map((t) => t.charAt(0).toUpperCase() + t.slice(1)).join(' ');
  }

  // Subagent model affix. Resolution order for Claude:
  //   1. parent.meta.subagent_model — stamped by the parser on the
  //      first subagent assistant envelope (most authoritative).
  //   2. payloadMeta.input.model — the user-supplied alias on the
  //      tool input (e.g. "opus"). Surfaces something for the brief
  //      window before the first subagent assistant message lands.
  //   3. omitted otherwise.
  let modelLabel = $derived.by<string>(() => {
    if ((parent.toolName ?? '') !== 'Agent') return '';
    const stamped = typeof parentMeta?.subagent_model === 'string' ? parentMeta.subagent_model : '';
    if (stamped) return displayModelLabel('claude', stamped);
    const requested = readString(inputObject, 'model');
    if (requested) return displayModelLabel('claude', requested);
    return '';
  });

  // Description stays on the first header line. The latest-action row below
  // is always present and swaps from "Initializing..." once child work lands.
  let inputDescription = $derived.by<string>(() => {
    const desc = readString(inputObject, 'description');
    if (desc) return desc;
    const prompt = readString(inputObject, 'prompt');
    if (prompt) return prompt.length > 80 ? `${prompt.slice(0, 80)}…` : prompt;
    return '';
  });

  let previewText = $derived.by<string>(() => {
    return group.latestChildSummary || 'Initializing...';
  });

  // ---- Status visualization (matches GenericToolCallRow) -----------

  let isBackgroundedLaunch = $derived(
    parent.kind === 'tool_call' && parent.isBackground === true,
  );

  let runningLabel = $derived.by<string | null>(() => {
    if (isBackgroundedLaunch) return '…';
    if (parent.status === 'running' || parent.status === 'streaming') return 'running';
    return null;
  });

  let now = $state(Date.now());
  $effect(() => {
    if (runningLabel === null) return;
    now = Date.now();
    const id = setInterval(() => {
      now = Date.now();
    }, 1_000);
    return () => clearInterval(id);
  });

  let elapsedLabel = $derived.by<string>(() => {
    const start = parent.createdAt;
    if (!Number.isFinite(start) || start <= 0) return '';
    const end = runningLabel !== null ? now : parent.updatedAt;
    if (!Number.isFinite(end) || end <= start) return '';
    return formatElapsedSeconds(Math.floor((end - start) / 1_000));
  });

  let completionStatus = $derived(
    deriveCompletionStatus(parent, { meta: payloadMeta }),
  );

  let entryCountLabel = $derived.by(() => {
    if (group.descendantCount === 0) return '1 agent';
    const entryLabel = `${group.descendantCount} ${group.descendantCount === 1 ? 'entry' : 'entries'}`;
    return `1 agent · ${entryLabel}`;
  });
  let entryCountAriaLabel = $derived.by(() => {
    if (group.descendantCount === 0) return '1 agent represented in this group';
    const entryLabel = `${group.descendantCount} ${group.descendantCount === 1 ? 'timeline entry' : 'timeline entries'}`;
    return `1 agent and ${entryLabel} inside this subagent group`;
  });
</script>

{#if showMarkerOnly}
  <div
    class="mb-2 flex items-center gap-2 text-xs italic text-text-secondary"
    style="margin-left: {indentRem}rem"
    data-testid="subagent-group-marker"
  >
    <span aria-hidden="true">↳</span>
    <span>Spawned subagent… ({entryCountLabel})</span>
  </div>
{:else}
  <div
    class="group/tool mb-1.5 overflow-hidden"
    style="margin-left: {indentRem}rem"
    data-testid="subagent-group"
    data-tool-kind="robot"
  >
    <TranscriptDisclosureHeader
      expanded={expanded}
      controls={`subagent-group-${parent.id}`}
      testId="subagent-group-toggle"
      class="rounded-[var(--radius-control)] px-1 py-1 hover:bg-surface-2/20"
      onToggle={() => toggle()}
    >
      <ToolKindIcon kind="robot" ariaLabel="Subagent" />
      <span class="min-w-0 flex-1">
        <span class="flex min-w-0 items-center gap-2">
          <span
            class="text-[11px] font-medium text-fg-muted shrink-0 uppercase tracking-[0.04em]"
            data-testid="subagent-group-label"
          >
            {label}{#if modelLabel}<span class="ml-1 text-fg-hint normal-case tracking-normal">({modelLabel})</span>{/if}
          </span>
          {#if inputDescription}
            <span class="min-w-0 truncate text-[12px] text-fg-muted/75" data-testid="subagent-group-description">
              {inputDescription}
            </span>
          {/if}
        </span>
        <span class="mt-0.5 block min-w-0 truncate text-[11px] text-fg-hint/85" data-testid="subagent-group-preview">
          <span aria-hidden="true">└</span>
          {previewText}
        </span>
      </span>
      {#snippet actions()}
        <span
          class="shrink-0 text-[10px] text-fg-hint opacity-70 transition-opacity group-hover/tool:opacity-100"
          data-testid="subagent-group-count"
          aria-label={entryCountAriaLabel}
        >
          {entryCountLabel}
        </span>
        <!-- Stable-width status slot: the running label and the
             completion badge have very different widths, so swapping
             one for the other on terminal transition would shift the
             entry-count and elapsed labels left/right. Reserving a
             min-width on the wrapper keeps the layout stable across
             the running → terminal flip. -->
        <span
          class="inline-flex shrink-0 items-center justify-end min-w-[3.5rem]"
          data-testid="subagent-group-status-slot"
        >
          {#if runningLabel !== null}
            {#if isBackgroundedLaunch}
              <span
                class="text-[20px] leading-none text-accent opacity-90 transition-opacity group-hover/tool:opacity-100"
                data-testid="subagent-group-status"
                data-status={parent.status}
                title="Running in background"
                aria-label="Backgrounded"
              >
                …
              </span>
            {:else}
              <span
                class="text-[10px] text-accent opacity-70 transition-opacity group-hover/tool:opacity-100"
                data-testid="subagent-group-status"
                data-status={parent.status}
              >
                {runningLabel}
              </span>
            {/if}
          {:else if completionStatus !== null}
            <CompletionBadge
              status={completionStatus}
              class="opacity-80 transition-opacity group-hover/tool:opacity-100"
            />
          {/if}
        </span>
        <!-- Always-rendered elapsed slot, even when the label string
             is empty. Without the reserved width, the slot only
             mounts after the first second tick, causing a one-time
             rightward layout shift of any trailing chrome. The
             tabular-nums + right-alignment keeps `0s → 1m 30s` from
             reflowing the row as digits roll over. -->
        <span
          class="shrink-0 tabular-nums text-[10px] text-fg-hint opacity-70 transition-opacity group-hover/tool:opacity-100 inline-block min-w-[3rem] text-right"
          data-testid="subagent-group-duration"
        >
          {elapsedLabel}
        </span>
      {/snippet}
    </TranscriptDisclosureHeader>

    {#if expanded}
      <div
        id="subagent-group-{parent.id}"
        class="ml-5 max-h-[20rem] overflow-y-auto border-l border-border-subtle bg-surface-0/35 px-3 py-2"
        role="region"
        aria-label="Subagent Timeline"
        data-testid="subagent-group-body"
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
