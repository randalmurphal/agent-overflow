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
  import { paneWorkspacePath } from '../../stores/thread.svelte';
  import { PROVIDER_DEFINITIONS } from '../../providers/catalog';
  import { parseJsonObject } from '../../utils/parseJsonObject';
  import { parsePatchFiles, type PatchFile, type PatchLine } from '../../utils/patchFiles';
  import AskUserQuestionCard from './AskUserQuestionCard.svelte';
  import CollabToolRow from './CollabToolRow.svelte';
  import CommandOutput from './CommandOutput.svelte';
  import DiffFileBlock from './DiffFileBlock.svelte';
  import DiffFileStack from './DiffFileStack.svelte';
  import GenericToolCallRow from './GenericToolCallRow.svelte';
  import ProposedPlanCard from './ProposedPlanCard.svelte';
  import ToolResultCard from './ToolResultCard.svelte';
  import { isCodexCollabControlToolName } from './codexCollabControls';
  import { commandTextForItem, isCommandToolName } from './commandDisplay';

  let {
    pane,
    item,
    codexSubagentReceiverLabels = new Map<string, string>(),
  }: {
    pane: ThreadPane;
    item: Item;
    codexSubagentReceiverLabels?: ReadonlyMap<string, string>;
  } = $props();

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
          preview: typeof parsedCmdMeta.preview === 'string' ? parsedCmdMeta.preview : undefined,
          errorMessage:
            typeof parsedCmdMeta.errorMessage === 'string' ? parsedCmdMeta.errorMessage : undefined,
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
          preview: cmdMeta?.preview,
          errorMessage: cmdMeta?.errorMessage,
        }
      : null,
  );
  let commandCollapsedPreview = $derived.by(() => {
    if (item.kind !== 'tool_completion' || !item.completionOf) return '';
    const meta = parseJsonObject(item.meta);
    const carrierID = meta?.wait_carrier_id ?? meta?.waitCarrierID;
    if (typeof carrierID !== 'string' || !carrierID.trim()) return '';
    return commandMeta?.preview ?? '';
  });
  let isCollabControlRow = $derived(
    pane.thread?.provider === PROVIDER_DEFINITIONS.codex.id
      && isCodexCollabControlToolName(item.toolName),
  );
  let isUserInputRow = $derived(
    item.toolName === 'AskUserQuestion' || item.toolName === 'request_user_input',
  );

  // Single-file PatchFile derived from a `payloadKind === 'diff'`
  // DiffMeta. The meta carries a complete (or sliced) unified-diff
  // text in `preview`; parsePatchFiles handles either single- or
  // multi-file patches but DiffMeta is single-file by contract.
  // Falls back to a header-only PatchFile if the preview wasn't a
  // parseable diff (rare; surfaces as a metadata-only row).
  let diffMetaPatchFile = $derived.by<PatchFile | null>(() => {
    if (!diffMeta) return null;
    const parsed = parsePatchFiles(diffMeta.preview);
    if (parsed.length > 0 && parsed[0]) return parsed[0];
    return {
      path: diffMeta.filePath,
      kind: diffMeta.changeKind,
      additions: diffMeta.insertions,
      deletions: diffMeta.deletions,
      lines: [] as PatchLine[],
    };
  });

  let hasInlineDiffFiles = $derived(
    Boolean(toolResultMeta?.inlineDiff?.files && toolResultMeta.inlineDiff.files.length > 0),
  );
</script>

{#if isUserInputRow}
  <!-- AskUserQuestion and request_user_input are UI tools with no real
       payload — questions live on item.meta.input.questions, answers
       are either persisted on item.meta.answers (Codex) or echoed via
       item.meta.tool_result.content (Claude). The dedicated card renders
       both with check/X marks per option. Branch must come BEFORE the
       generic payloadKind dispatch so answer blobs do not route to an
       unrelated specialised renderer. -->
  <AskUserQuestionCard {pane} {item} />
{:else if isCollabControlRow}
  <CollabToolRow {pane} {item} {codexSubagentReceiverLabels} />
{:else if planMeta && payloadId}
  <ProposedPlanCard {pane} {item} {payloadId} meta={planMeta} />
{:else if diffMetaPatchFile}
  <!-- Single-file diff payload (Claude legacy `payloadKind=diff`,
       per-turn EventDiff upgrade). The patch text is in the meta's
       preview field; no payload fetch is needed for inline render. -->
  <DiffFileBlock
    {pane}
    file={diffMetaPatchFile}
    {payloadId}
    threadId={item.threadId}
    workspacePath={paneWorkspacePath(pane)}
    toolName={item.toolName}
  />
{:else if toolResultMeta && hasInlineDiffFiles}
  <!-- Multi-file diff (Claude Edit/Write/MultiEdit/NotebookEdit;
       Codex apply_patch with N files). One DiffFileBlock per file,
       no outer wrapper — each file is its own self-contained row.
       DiffFileStack handles the lazy payload fetch and per-file
       slicing via parsePatchFiles + path-match. -->
  <DiffFileStack {pane} {item} meta={toolResultMeta} {payloadId} />
{:else if toolResultMeta}
  <!-- Non-diff tool_result fallthrough: an entry that lacks an
       inlineDiff.files — kept for any legacy producer that emits
       a ToolResultMeta with detail/preview text only. -->
  <ToolResultCard {pane} {item} meta={toolResultMeta} {payloadId} />
{:else if isCommandRow}
  <CommandOutput {pane} {item} meta={commandMeta} {payloadId} collapsedPreview={commandCollapsedPreview} />
{:else}
  <GenericToolCallRow {pane} {item} />
{/if}
