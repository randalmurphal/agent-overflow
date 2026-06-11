<script lang="ts">
  // ToolCallCard dispatches a `tool_call` / `tool_completion` item to the
  // correct renderer. Structured payloads go to their rich components; all
  // other tools use GenericToolCallRow's lightweight header/body.

  import type { Item } from '../../types/models';
  import type { ThreadPane } from '../../stores/thread.svelte';
  import { paneWorkspacePath } from '../../stores/thread.svelte';
  import AdvisorRow from './AdvisorRow.svelte';
  import AgentRow from './AgentRow.svelte';
  import AskUserQuestionCard from './AskUserQuestionCard.svelte';
  import CollabToolRow from './CollabToolRow.svelte';
  import CommandOutput from './CommandOutput.svelte';
  import DiffFileBlock from './DiffFileBlock.svelte';
  import DiffFileStack from './DiffFileStack.svelte';
  import GenericToolCallRow from './GenericToolCallRow.svelte';
  import ProposedPlanCard from './ProposedPlanCard.svelte';
  import ToolResultCard from './ToolResultCard.svelte';
  import { resolveToolPresentation } from './toolPresentation';

  let {
    pane,
    item,
    codexSubagentReceiverLabels = new Map<string, string>(),
  }: {
    pane: ThreadPane;
    item: Item;
    codexSubagentReceiverLabels?: ReadonlyMap<string, string>;
  } = $props();

  let presentation = $derived(
    resolveToolPresentation({
      item,
      provider: pane.thread?.provider,
      surface: 'timeline',
    }),
  );
</script>

{#if presentation.kind === 'user-input'}
  <!-- AskUserQuestion and request_user_input are UI tools with no real
       payload — questions live on item.meta.input.questions, answers
       are either persisted on item.meta.answers (Codex) or echoed via
       item.meta.tool_result.content (Claude). The dedicated card renders
       both with check/X marks per option. Branch must come BEFORE the
       generic payloadKind dispatch so answer blobs do not route to an
       unrelated specialised renderer. -->
  <AskUserQuestionCard {pane} {item} />
{:else if presentation.kind === 'collab'}
  <CollabToolRow {pane} {item} {codexSubagentReceiverLabels} />
{:else if presentation.kind === 'proposed-plan'}
  <ProposedPlanCard {pane} {item} meta={presentation.meta} />
{:else if presentation.kind === 'single-file-diff'}
  <!-- Single-file diff payload (Claude legacy `payloadKind=diff`,
       per-turn EventDiff upgrade). The patch text is in the meta's
       preview field; no payload fetch is needed for inline render. -->
  <DiffFileBlock
    {pane}
    file={presentation.file}
    payloadId={presentation.payloadId}
    threadId={item.threadId}
    itemId={item.id}
    workspacePath={paneWorkspacePath(pane)}
    toolName={item.toolName}
    createdAt={item.createdAt}
  />
{:else if presentation.kind === 'diff-stack'}
  <!-- Multi-file diff (Claude Edit/Write/MultiEdit/NotebookEdit;
       Codex apply_patch with N files). One DiffFileBlock per file,
       no outer wrapper — each file is its own self-contained row.
       DiffFileStack handles the lazy payload fetch and per-file
       slicing via parsePatchFiles + path-match. -->
  <DiffFileStack {pane} {item} meta={presentation.meta} payloadId={presentation.payloadId} />
{:else if presentation.kind === 'tool-result'}
  <!-- Non-diff tool_result fallthrough: an entry that lacks an
       inlineDiff.files — kept for any legacy producer that emits
       a ToolResultMeta with detail/preview text only. -->
  <ToolResultCard {pane} {item} meta={presentation.meta} payloadId={presentation.payloadId} />
{:else if presentation.kind === 'command'}
  <CommandOutput
    {pane}
    item={presentation.item}
    meta={presentation.meta}
    payloadId={presentation.payloadId}
    collapsedPreview={presentation.collapsedPreview}
  />
{:else if presentation.kind === 'agent'}
  <AgentRow {pane} {item} />
{:else if presentation.kind === 'advisor'}
  <!-- Claude's server-side `advisor` tool call. Runs inline (not
       backgrounded) with its own model context window; the response
       text body is shipped as the tool_call_result payload and the
       parent envelope's model id is stamped on item.meta.advisor_model
       (rendered via displayModelLabel). -->
  <AdvisorRow {pane} {item} />
{:else}
  <GenericToolCallRow {pane} {item} />
{/if}
