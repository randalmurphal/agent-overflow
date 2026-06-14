<script lang="ts">
  // The live "compact" tail: the claudetui compaction summarizer's reasoning,
  // streamed as its own top-level row just above the "Context compacted"
  // divider. Thin config wrapper over ReasoningTailRow (shared with
  // ThinkingBlock), diverging only in icon/label, ids, and its own
  // payload-expansion namespace so a reasoning row and its sibling divider never
  // collide. The committed compaction summary is a separate concern and lives on
  // CompactionDivider.
  import type { Item } from '../../types/models';
  import type { ThreadPane } from '../../stores/thread.svelte';
  import { COMPACTION_REASONING_PAYLOAD_EXPANSION_STATE_KEY } from '../../utils/payloadVersion';
  import ReasoningTailRow from './ReasoningTailRow.svelte';

  let { pane, item }: { pane?: ThreadPane; item: Item } = $props();
</script>

<ReasoningTailRow
  {pane}
  {item}
  stateKey={COMPACTION_REASONING_PAYLOAD_EXPANSION_STATE_KEY}
  iconKind="compaction"
  iconAriaLabel="compact"
  labelText="compact"
  idPrefix="compaction-reasoning"
  toggleAriaLabel="Toggle Compaction Reasoning"
  copyLabel="Copy reasoning"
/>
