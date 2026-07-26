<script lang="ts">
  /*
   * AskUserQuestionCard renders persisted timeline rows for Claude
   * AskUserQuestion and Codex request_user_input tool calls. The live
   * interaction lives in the in-composer panel
   * (ComposerPendingUserInputPanel); this card is the historical record
   * that survives across reloads, forks, and restores — the user can
   * come back later and see what they were asked and what they answered.
   *
   * Data sources:
   * - `item.meta.input.questions` — the questions array, set by the
   *   tool_use launch event. Always present.
   * - `item.meta.answers` — Codex's persisted answers, set when AO
   *   resolves the synthetic request_user_input tool call.
   * - `item.meta.tool_result.content` — Claude's echo of the user's
   *   answers, set by the tool_result completion event. Present once
   *   the user has answered. Parsed from Claude's canonical
   *   `"question"="answer"` sentence, with JSON kept for legacy rows.
   *
   * Status rendering reuses `deriveCompletionStatus` so the row state
   * stays consistent with every other tool card. Running shows an
   * indicator dot, answered renders no success chrome, and failures
   * show an error indicator plus RowError.
   */
  import { untrack } from 'svelte';
  import Check from 'lucide-svelte/icons/check';
  import X from 'lucide-svelte/icons/x';
  import Icon from '../primitives/Icon.svelte';
  import ChatMarkdown from './ChatMarkdown.svelte';
  import type { Item } from '../../types/models';
  import { chatRowDomId } from '../../utils/chatDomIds';
  import { paneWorkspacePath, type ThreadPane } from '../../stores/thread.svelte';
  import { deriveCompletionStatus } from '../../utils/toolCompletionStatus';
  import { formatTimeOfDay } from '../../utils/format';
  import { parseJsonObject } from '../../utils/parseJsonObject';
  import { createPayloadExpansion } from '../../utils/payloadExpansion.svelte';
  import TranscriptDisclosureHeader from './TranscriptDisclosureHeader.svelte';
  import ToolHeaderMeta from './ToolHeaderMeta.svelte';
  import ToolKindIcon from './ToolKindIcon.svelte';
  import ToolRowStatusIndicator from './ToolRowStatusIndicator.svelte';
  import RowError from './RowError.svelte';
  import { indicatorStateForItem, rowErrorForStatus } from './rowState';
  import {
    answersForQuestion as answersForQuestionFromMap,
    classifyAnswers,
    extractAnswers,
    extractQuestions,
    headerLabelForQuestions,
    type AskQuestion,
  } from './askUserQuestionData';
  import { preservePaneScrollAnchor } from './preserveScrollAnchor';
  import { getPathRefsFromMeta } from '../../utils/pathLinkify';
  import { useLeasedItemExpansion } from './useLeasedPayloadExpansion.svelte';
  import { nestedScroll } from '../../utils/scroll/wheelAttribution';

  let { pane, item }: { pane?: ThreadPane; item: Item } = $props();

  // Expansion state lives on the per-pane registry so it survives
  // windowing remount when the row scrolls out of the buffer and back
  // (per the row contract documented in components/chat/AGENTS.md).
  // The handle's payload-fetching codepath is a no-op for AskUserQuestion
  // rows — they have no `payloadId` (questions + answers live on
  // `item.meta` and arrive synchronously) — but the boolean `expanded`
  // flag survives remount, which is what we need.
  // Local fallback covers tests / surfaces that haven't plumbed `pane`.
  const localFallback = untrack(() =>
    pane
      ? null
      : createPayloadExpansion(
          () => item.payloadId,
          () => item.threadId,
          { payloadVersion: () => item.updatedAt },
        ),
  );
  const expansionRef = useLeasedItemExpansion({
    getPane: () => pane,
    getItem: () => item,
    getFallback: () => localFallback,
  });
  // One derived id for both halves of the disclosure (utils/chatDomIds.ts):
  // the header's `controls` and the body's `id` must be one string.
  let bodyDomId = $derived(chatRowDomId(pane, 'ask-user-question-body', item.id));
  const expansion = $derived(expansionRef.current!);

  async function toggle() {
    await expansion.toggle();
  }

  const itemMeta = $derived(parseJsonObject(item.meta));

  // pathRefs is stamped onto the AskUserQuestion / request_user_input
  // item.meta at persistToolCallLaunch time. The same allowlist applies
  // to every question/option body the card renders through ChatMarkdown
  // — they all flowed through the validator off the same workspace.
  const pathRefs = $derived(getPathRefsFromMeta(item.meta) ?? []);

  const questions = $derived.by<AskQuestion[]>(() => {
    return extractQuestions(itemMeta);
  });

  // Answers are persisted directly on `item.meta.answers` for both providers:
  // Codex on its request_user_input row, Claude when triage merges the resolved
  // answers onto the AskUserQuestion launch row. Claude's `tool_result.content`
  // echo is a secondary, best-effort source. Parse defensively — if a shape is
  // unexpected we just render no answers (the row still shows the questions).
  // `directAnswers` win: they are exactly what was sent to the agent, whereas
  // the echo is free-form text that may not parse.
  const answersByQuestion = $derived.by<Record<string, string | string[]>>(() => {
    if (!itemMeta) return {};
    const directAnswers = extractAnswers((itemMeta as Record<string, unknown>).answers);
    const toolResult = itemMeta.tool_result;
    if (!toolResult || typeof toolResult !== 'object') return directAnswers;
    const toolResultAnswers = extractAnswers((toolResult as Record<string, unknown>).content);
    return { ...toolResultAnswers, ...directAnswers };
  });

  const headerLabel = $derived.by<string>(() => {
    return headerLabelForQuestions(questions);
  });

  const completionStatus = $derived(deriveCompletionStatus(item));
  const isRunningQuestion = $derived(item.status === 'running' || item.status === 'streaming');
  const indicatorState = $derived(indicatorStateForItem(item));
  const rowError = $derived.by(() => {
    if (completionStatus !== 'failure') return null;
    return rowErrorForStatus(item.status, 'Question was not answered') ?? {
      tone: 'error' as const,
      msg: 'Question was not answered',
    };
  });

  let time = $derived(formatTimeOfDay(item.createdAt));

  function answersForQuestion(q: AskQuestion): string[] {
    return answersForQuestionFromMap(q, answersByQuestion);
  }

</script>

<div
  class="group/tool overflow-hidden"
  data-testid="ask-user-question-card"
  data-status={item.status}
>
  <TranscriptDisclosureHeader
    expanded={expansion.expanded}
    controls={bodyDomId}
    testId="ask-user-question-toggle"
    class="rounded-[var(--radius-control)] px-1 py-1 hover:bg-surface-2/20"
    onToggle={(event) => preservePaneScrollAnchor(pane, event, toggle)}
  >
    {#snippet icon()}<ToolKindIcon kind="speech-bubble" ariaLabel="ask" />{/snippet}
    {#snippet label()}<span data-testid="ask-user-question-label">ask</span>{/snippet}
    {#snippet body()}
      <span class="min-w-0 flex-1 truncate text-[0.75rem] text-fg-muted/75" data-testid="ask-user-question-title">
        {headerLabel}
      </span>
    {/snippet}
    {#snippet actions()}
      <ToolHeaderMeta
        statusSlotTestId="ask-user-question-status-slot"
        duration={{ testId: 'ask-user-question-duration', label: '' }}
        timestamp={{ testId: 'ask-user-question-time', value: item.createdAt, label: time }}
      >
        {#snippet status()}
          <ToolRowStatusIndicator
            {item}
            state={isRunningQuestion || completionStatus === 'failure' ? indicatorState : null}
            testId="ask-user-question-status"
          />
        {/snippet}
      </ToolHeaderMeta>
    {/snippet}
  </TranscriptDisclosureHeader>

  {#if rowError}
    <div class="ml-[5.25rem] px-3 pb-1">
      <RowError tone={rowError.tone} msg={rowError.msg} />
    </div>
  {/if}

  {#if expansion.expanded}
    <!-- No transition: the timeline scroll surface forbids height-shifting
         transitions adjacent to it (frontend/AGENTS.md "Anti-patterns").
         The chevron rotation on the toggle is enough visual feedback. -->
    <div
      id={bodyDomId}
      class="ml-5 border-l border-border-subtle bg-surface-0/35 px-3 py-2"
      data-testid="ask-user-question-body"
    >
      {#if questions.length === 0}
        <p class="text-[0.6875rem] text-fg-subtle italic">
          No question metadata stored on this row.
        </p>
      {:else}
        <ul class="space-y-3">
          {#each questions as q, qIndex}
            {@const answers = answersForQuestion(q)}
            {@const { matched, customs } = classifyAnswers(q, answers)}
            <li class="space-y-1.5" data-testid="ask-user-question-question-{qIndex}">
              {#if q.header}
                <p class="text-[0.625rem] font-semibold uppercase tracking-[0.06em] text-fg-muted">
                  {q.header}
                </p>
              {/if}
              <div class="text-xs text-fg" data-testid="ask-user-question-prompt">
                <ChatMarkdown
                  source={q.question}
                  workspacePath={paneWorkspacePath(pane)}
                  {pathRefs}
                />
              </div>
              {#if q.options && q.options.length > 0}
                <ul class="ml-2 space-y-1">
                  {#each q.options as option (option.label)}
                    {@const isSelected = matched.has(option.label)}
                    <li
                      class="flex items-start gap-2 text-[0.6875rem]"
                      data-testid="ask-user-question-option"
                      data-selected={isSelected ? 'true' : 'false'}
                    >
                      {#if isSelected}
                        <span class="mt-0.5 shrink-0 text-success" aria-label="Selected">
                          <Icon icon={Check} size={12} />
                        </span>
                      {:else}
                        <span class="mt-0.5 shrink-0 text-fg-subtle" aria-label="Not selected">
                          <Icon icon={X} size={12} />
                        </span>
                      {/if}
                      <div class="min-w-0 flex-1">
                        <p class={isSelected ? 'text-fg' : 'text-fg-muted'}>
                          {option.label}
                        </p>
                        {#if option.description}
                          <p class="text-[0.65625rem] text-fg-subtle">{option.description}</p>
                        {/if}
                        {#if isSelected && option.preview?.trim()}
                          <div
                            class="mt-1 max-h-40 overflow-y-auto rounded border border-border-subtle bg-surface-0 px-2 py-1"
                            use:nestedScroll
                            data-testid="ask-user-question-preview"
                          >
                            <ChatMarkdown
                              source={option.preview}
                              workspacePath={paneWorkspacePath(pane)}
                              {pathRefs}
                              class="text-[0.6875rem]"
                            />
                          </div>
                        {/if}
                      </div>
                    </li>
                  {/each}
                </ul>
              {/if}
              {#each customs as customAnswer}
                <div
                  class="ml-2 flex items-start gap-2 text-[0.6875rem]"
                  data-testid="ask-user-question-custom"
                >
                  <span class="mt-0.5 shrink-0 text-success" aria-label="Custom answer">
                    <Icon icon={Check} size={12} />
                  </span>
                  <p class="min-w-0 flex-1 text-fg">
                    <span class="text-[0.65625rem] font-semibold uppercase tracking-[0.06em] text-fg-muted">Custom:</span>
                    <span class="ml-1">{customAnswer}</span>
                  </p>
                </div>
              {/each}
              {#if answers.length === 0 && !isRunningQuestion}
                <p class="ml-2 text-[0.6875rem] italic text-fg-subtle">
                  No answer recorded.
                </p>
              {/if}
            </li>
          {/each}
        </ul>
      {/if}
    </div>
  {/if}
</div>
