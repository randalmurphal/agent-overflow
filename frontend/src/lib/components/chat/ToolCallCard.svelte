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
  import { parseJsonObject } from '../../utils/parseJsonObject';
  import AskUserQuestionCard from './AskUserQuestionCard.svelte';
  import CollabToolRow from './CollabToolRow.svelte';
  import CommandOutput from './CommandOutput.svelte';
  import DiffPreview from './DiffPreview.svelte';
  import GenericToolCallRow from './GenericToolCallRow.svelte';
  import ProposedPlanCard from './ProposedPlanCard.svelte';
  import ToolResultCard from './ToolResultCard.svelte';
  import { isCodexCollabControlToolName } from './codexCollabControls';
  import { commandTextForItem, isCommandToolName } from './commandDisplay';

  let { pane, item }: { pane: ThreadPane; item: Item } = $props();

  let payloadKind = $derived(item.payloadKind);
  let payloadId = $derived(item.payloadId);

  let planMeta = $derived<ProposedPlanMeta | null>(
    payloadKind === 'proposed_plan' && payloadId
      ? (parseJsonObject(item.payloadMeta) as ProposedPlanMeta | null)
      : null,
  );
  let diffMeta = $derived<DiffMeta | null>(
    payloadKind === 'diff' && payloadId
      ? (parseJsonObject(item.payloadMeta) as DiffMeta | null)
      : null,
  );
  let parsedCmdMeta = $derived(
    payloadKind === 'command_output'
      ? (parseJsonObject(item.payloadMeta) as Partial<CommandOutputMeta> | null)
      : null,
  );
  let cmdMeta = $derived<CommandOutputMeta | null>(
    parsedCmdMeta
      ? {
          command: typeof parsedCmdMeta.command === 'string' ? parsedCmdMeta.command : '',
          exitCode: typeof parsedCmdMeta.exitCode === 'number' ? parsedCmdMeta.exitCode : 0,
          lineCount: typeof parsedCmdMeta.lineCount === 'number' ? parsedCmdMeta.lineCount : 0,
          preview: typeof parsedCmdMeta.preview === 'string' ? parsedCmdMeta.preview : '',
        }
      : null,
  );
  let toolResultMeta = $derived<ToolResultMeta | null>(
    payloadKind === 'tool_result'
      ? (parseJsonObject(item.payloadMeta) as ToolResultMeta | null)
      : null,
  );
  let isCommandRow = $derived(
    payloadKind === 'command_output' ||
      isCommandToolName(item.toolName) ||
      isCommandToolName(item.summary?.split(':', 1)[0]),
  );
  let commandMeta = $derived<CommandOutputMeta | null>(
    isCommandRow
      ? {
          command: commandTextForItem(item, cmdMeta),
          exitCode: cmdMeta?.exitCode ?? 0,
          lineCount: cmdMeta?.lineCount ?? 0,
          preview: cmdMeta?.preview ?? '',
        }
      : null,
  );
  let isCollabControlRow = $derived(
    pane.thread?.provider === 'codex' && isCodexCollabControlToolName(item.toolName),
  );
</script>

{#if item.toolName === 'AskUserQuestion'}
  <!-- AskUserQuestion is a UI tool with no real payload — questions live
       on item.meta.input.questions, answers come back via the
       tool_result echo on item.meta.tool_result.content. The dedicated
       card renders both with check/X marks per option. Branch must come
       BEFORE the generic payloadKind dispatch so the JSON-string
       answers don't accidentally route to a wrong specialised renderer. -->
  <AskUserQuestionCard {pane} {item} />
{:else if isCollabControlRow}
  <CollabToolRow {item} />
{:else if planMeta && payloadId}
  <ProposedPlanCard {pane} {item} {payloadId} meta={planMeta} />
{:else if diffMeta && payloadId}
  <DiffPreview {pane} {item} meta={diffMeta} {payloadId} />
{:else if toolResultMeta}
  <!-- File-change / command-mutation helpers mark lifecycle rows as
       tool_result; render the rich card even before a payload id exists so
       exact-patch rows keep a stable disabled disclosure shell. Gating on
       payloadKind (not just a successful JSON parse) avoids tool_call_result
       payloads coincidentally matching the ToolResultMeta shape. -->
  <ToolResultCard {pane} {item} meta={toolResultMeta} {payloadId} />
{:else if isCommandRow}
  <CommandOutput {pane} {item} meta={commandMeta} {payloadId} />
{:else}
  <GenericToolCallRow {pane} {item} />
{/if}
