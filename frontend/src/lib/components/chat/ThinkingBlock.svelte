<script lang="ts">
  // The model's reasoning, streamed as a tail-clamped disclosure row. Thin
  // config wrapper over ReasoningTailRow (shared with CompactionReasoning) — the
  // expansion wiring, live-tail sourcing, and hover-reveal group live there.
  // This wrapper only supplies the think-specific icon, label, ids, and
  // payload-expansion namespace.
  import type { Item } from '../../types/models';
  import type { ThreadPane } from '../../stores/thread.svelte';
  import { THINKING_PAYLOAD_EXPANSION_STATE_KEY } from '../../utils/payloadVersion';
  import ReasoningTailRow from './ReasoningTailRow.svelte';

  let { pane, item }: { pane?: ThreadPane; item: Item } = $props();
</script>

<ReasoningTailRow
  {pane}
  {item}
  stateKey={THINKING_PAYLOAD_EXPANSION_STATE_KEY}
  iconKind="brain"
  iconAriaLabel="think"
  labelText="think"
  idPrefix="thinking"
  toggleAriaLabel="Toggle Thinking Block"
  copyLabel="Copy thinking"
/>
