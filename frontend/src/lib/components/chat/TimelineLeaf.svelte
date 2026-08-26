<script lang="ts">
  import type { ThreadPane } from '../../stores/thread.svelte';
  import type { Item } from '../../types/models';
  import APIErrorRow from './APIErrorRow.svelte';
  import APIRetryRow from './APIRetryRow.svelte';
  import AssistantMessage from './AssistantMessage.svelte';
  import CompactionDivider from './CompactionDivider.svelte';
  import CompactionReasoning from './CompactionReasoning.svelte';
  import CommandResultRow from './CommandResultRow.svelte';
  import NotificationRow from './NotificationRow.svelte';
  import SessionDiedNotification from './SessionDiedNotification.svelte';
  import TerminalInteractionRow from './TerminalInteractionRow.svelte';
  import ThinkingBlock from './ThinkingBlock.svelte';
  import ToolCallCard from './ToolCallCard.svelte';
  import UserMessage from './UserMessage.svelte';
  import { parseJsonObject } from '../../utils/parseJsonObject';
  import type { ExpandedImagePreview } from '../../utils/attachmentPreview.svelte';
  import type { UserMessageActions } from './userMessageActions';

  let {
    pane,
    item,
    orphan = false,
    onImageExpand,
    userMessageActions,
    codexSubagentReceiverLabels = new Map<string, string>(),
  }: {
    pane: ThreadPane;
    item: Item;
    orphan?: boolean;
    onImageExpand?: (preview: ExpandedImagePreview) => void;
    userMessageActions?: UserMessageActions;
    codexSubagentReceiverLabels?: ReadonlyMap<string, string>;
  } = $props();

  // Resolve by id at the row boundary so ordinary leaf streams can update
  // this row without forcing MessageTimeline to rebuild the whole
  // virtualizer data array. The prop remains the structural fallback for the
  // brief teardown / prune window where the row outlives the pane item.
  const displayItem = $derived(pane.getItemById(item.id) ?? item);

  // Notification kind is encoded on meta.kind for sub-discrimination —
  // session-died notifications carry their own renderer (the historical
  // record of a process exit). Plain `notification` rows fall back to
  // NotificationRow's generic shape.
  const notificationKind = $derived.by(() => {
    if (displayItem.kind !== 'notification') return '';
    const meta = parseJsonObject(displayItem.meta);
    return typeof meta?.kind === 'string' ? meta.kind : '';
  });
</script>

<!-- kind / role / status ride the row root beside the id because the
     harness bridge's viewport snapshot (lib/harness/snapshot.ts) has to
     answer "what is on screen" WITHOUT re-deriving item semantics from
     class names or renderer shapes. They are the row's own discriminators
     (frontend/AGENTS.md: discriminate by `kind`, never role or content),
     read straight off the resolved item so the DOM cannot disagree with
     what the pane thinks it rendered. Three attributes, no state, no
     cost: keep it that way rather than growing a per-renderer vocabulary
     here. -->
<div
  data-item-id={displayItem.id}
  data-item-kind={displayItem.kind}
  data-item-role={displayItem.role}
  data-item-status={displayItem.status}
>
  {#if orphan}
    <div
      class="mb-1 flex items-center gap-2 text-xs text-warning"
      role="status"
      aria-label="Orphan Subagent Item"
    >
      <span aria-hidden="true">⚠</span>
      <span>Orphan subagent entry — parent tool call not found.</span>
    </div>
  {/if}
  <!-- AskUserQuestion no longer suppressed: the backend now persists
       it as a normal tool_call → tool_completion lifecycle. The
       in-composer ComposerPendingUserInputPanel still drives the live
       interaction; the timeline row (rendered by AskUserQuestionCard
       inside ToolCallCard) is the persisted historical record that
       survives reloads, forks, and restores. See
       internal/provider/claude/parse_assistant.go and parse_user.go. -->
  {#if displayItem.kind === 'user_text'}
    <UserMessage
      {pane}
      item={displayItem}
      {onImageExpand}
      actions={userMessageActions}
    />
  {:else if displayItem.kind === 'tool_call' || displayItem.kind === 'tool_completion'}
    <ToolCallCard {pane} item={displayItem} {codexSubagentReceiverLabels} />
  {:else if displayItem.kind === 'thinking'}
    <ThinkingBlock {pane} item={displayItem} />
  {:else if displayItem.kind === 'compaction_reasoning'}
    <CompactionReasoning {pane} item={displayItem} />
  {:else if displayItem.kind === 'command_result'}
    <!-- Output of a CLI-executed slash command. Explicit branch, because the
         fallback below is the assistant bubble and this row has no model
         behind it at all. -->
    <CommandResultRow {pane} item={displayItem} />
  {:else if displayItem.kind === 'terminal_interaction'}
    <TerminalInteractionRow {pane} item={displayItem} />
  {:else if displayItem.kind === 'notification'}
    {#if notificationKind === 'session_died'}
      <SessionDiedNotification item={displayItem} />
    {:else}
      <NotificationRow item={displayItem} />
    {/if}
  {:else if displayItem.kind === 'api_retry'}
    <APIRetryRow item={displayItem} />
  {:else if displayItem.kind === 'api_error'}
    <APIErrorRow item={displayItem} />
  {:else if displayItem.kind === 'error'}
    <div class="mb-4 rounded-[var(--radius-control)] border border-error/30 bg-error/10 px-3 py-2 text-sm text-error">
      {displayItem.summary}
    </div>
  {:else if displayItem.kind === 'compaction'}
    <CompactionDivider {pane} item={displayItem} />
  {:else}
    <AssistantMessage {pane} item={displayItem} />
  {/if}
</div>
