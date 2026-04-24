<script lang="ts">
  // ToolCallCard dispatches a `tool_call` / `tool_completion` item to the
  // correct renderer. Structured payloads go to their rich components; all
  // other tools use GenericToolCallRow's lightweight header/body.

  import type {
    CommandOutputMeta,
    DiffMeta,
    Item,
    ProposedPlanMeta,
    ToolResultMeta,
  } from '../../types/models';
  import type { ThreadPane } from '../../stores/thread.svelte';
  import CommandOutput from './CommandOutput.svelte';
  import DiffPreview from './DiffPreview.svelte';
  import GenericToolCallRow from './GenericToolCallRow.svelte';
  import ProposedPlanCard from './ProposedPlanCard.svelte';
  import ToolResultCard from './ToolResultCard.svelte';

  let { pane, item }: { pane: ThreadPane; item: Item } = $props();

  function parseMeta<T>(raw: string | undefined): T | null {
    if (!raw) return null;
    try {
      return JSON.parse(raw) as T;
    } catch {
      return null;
    }
  }

  let payloadKind = $derived(item.payloadKind);
  let payloadId = $derived(item.payloadId);

  let planMeta = $derived<ProposedPlanMeta | null>(
    payloadKind === 'proposed_plan' && payloadId ? parseMeta<ProposedPlanMeta>(item.payloadMeta) : null,
  );
  let diffMeta = $derived<DiffMeta | null>(
    payloadKind === 'diff' && payloadId ? parseMeta<DiffMeta>(item.payloadMeta) : null,
  );
  let cmdMeta = $derived<CommandOutputMeta | null>(
    payloadKind === 'command_output' && payloadId ? parseMeta<CommandOutputMeta>(item.payloadMeta) : null,
  );
  let toolResultMeta = $derived<ToolResultMeta | null>(
    payloadKind === 'tool_result' && payloadId ? parseMeta<ToolResultMeta>(item.payloadMeta) : null,
  );
</script>

{#if planMeta && payloadId}
  <ProposedPlanCard {pane} {payloadId} meta={planMeta} />
{:else if diffMeta && payloadId}
  <DiffPreview {item} meta={diffMeta} {payloadId} />
{:else if cmdMeta && payloadId}
  <CommandOutput {item} meta={cmdMeta} {payloadId} />
{:else if toolResultMeta && payloadId}
  <!-- File-change / command-mutation helpers attach a tool_result payload
       to the lifecycle row; render the rich diff card so file edits keep
       their existing visual weight. Gating on payloadKind (not just a
       successful JSON parse) avoids tool_call_result payloads coincidentally
       matching the ToolResultMeta shape and rendering as an empty card. -->
  <ToolResultCard {item} meta={toolResultMeta} {payloadId} />
{:else}
  <GenericToolCallRow {item} />
{/if}
