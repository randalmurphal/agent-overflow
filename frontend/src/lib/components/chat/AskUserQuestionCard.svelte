<script lang="ts">
  /*
   * AskUserQuestionCard renders the persisted timeline row for a Claude
   * AskUserQuestion tool call. The live interaction lives in the
   * in-composer panel (ComposerPendingUserInputPanel); this card is the
   * historical record that survives across reloads, forks, and restores
   * — the user can come back later and see what they were asked and
   * what they answered.
   *
   * Data sources:
   * - `item.meta.input.questions` — the questions array, set by the
   *   tool_use launch event. Always present.
   * - `item.meta.tool_result.content` — Claude's echo of the user's
   *   answers, set by the tool_result completion event. Present once
   *   the user has answered. Parsed as JSON `{answers, questions}` (the
   *   shape we sent via RespondToUserInput).
   *
   * Status rendering reuses `deriveCompletionStatus` so the badge logic
   * stays consistent with every other tool card. Running shows the
   * "running" pill; answered shows green check; force-closed-by-interrupt
   * (status='errored' from `forceCloseOrphanToolCalls`) shows red.
   */
  import { untrack } from 'svelte';
  import Check from 'lucide-svelte/icons/check';
  import X from 'lucide-svelte/icons/x';
  import ChevronRight from 'lucide-svelte/icons/chevron-right';
  import HelpCircle from 'lucide-svelte/icons/help-circle';
  import Icon from '../primitives/Icon.svelte';
  import CompletionBadge from './CompletionBadge.svelte';
  import ChatMarkdown from './ChatMarkdown.svelte';
  import type { Item } from '../../types/models';
  import type { ThreadPane } from '../../stores/thread.svelte';
  import { deriveCompletionStatus } from '../../utils/toolCompletionStatus';
  import { parseJsonObject } from '../../utils/parseJsonObject';
  import { createPayloadExpansion } from './payloadExpansion.svelte';

  let { pane, item }: { pane?: ThreadPane; item: Item } = $props();

  interface AskOption {
    label: string;
    description?: string;
    preview?: string;
  }
  interface AskQuestion {
    id?: string;
    header?: string;
    question: string;
    multiSelect?: boolean;
    options?: AskOption[];
  }

  // Expansion state lives on the per-pane registry so it survives
  // virtua remount when the row scrolls past `bufferSize=900` and back
  // (per the row contract documented in components/chat/CLAUDE.md).
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
  const expansion = $derived(pane ? pane.expansionStateFor(item) : localFallback!);

  async function toggle() {
    await expansion.toggle();
  }

  async function handleKeydown(evt: KeyboardEvent) {
    if (evt.key === 'Enter' || evt.key === ' ') {
      evt.preventDefault();
      await toggle();
    }
  }

  const itemMeta = $derived(parseJsonObject(item.meta));

  const questions = $derived.by<AskQuestion[]>(() => {
    if (!itemMeta) return [];
    const input = itemMeta.input;
    if (!input || typeof input !== 'object') return [];
    const list = (input as Record<string, unknown>).questions;
    if (!Array.isArray(list)) return [];
    return list.filter((q): q is AskQuestion => !!q && typeof q === 'object' && typeof (q as Record<string, unknown>).question === 'string');
  });

  // Answers are echoed back inside `tool_result.content` after the user
  // submits. The content is whatever shape Claude's CLI synthesizes for
  // an AskUserQuestion tool_result; the most common shape is a JSON
  // string of `{answers: {questionId: string | string[]}}`. Parse
  // defensively — if the shape is unexpected we just render no answers
  // (the row still shows the questions that were asked).
  const answersByQuestion = $derived.by<Record<string, string | string[]>>(() => {
    if (!itemMeta) return {};
    const toolResult = itemMeta.tool_result;
    if (!toolResult || typeof toolResult !== 'object') return {};
    const content = (toolResult as Record<string, unknown>).content;
    return extractAnswers(content);
  });

  function extractAnswers(content: unknown): Record<string, string | string[]> {
    if (typeof content === 'string') {
      return parseAnswersString(content);
    }
    if (Array.isArray(content)) {
      // Tool result content can be a list of `{type, text}` blocks; we
      // concatenate the text parts and try to parse the result.
      const text = content
        .map((part) => {
          if (part && typeof part === 'object' && typeof (part as Record<string, unknown>).text === 'string') {
            return (part as Record<string, unknown>).text as string;
          }
          return '';
        })
        .join('');
      return parseAnswersString(text);
    }
    return {};
  }

  function parseAnswersString(text: string): Record<string, string | string[]> {
    const trimmed = text.trim();
    if (!trimmed) return {};
    try {
      const parsed = JSON.parse(trimmed) as unknown;
      if (parsed && typeof parsed === 'object' && !Array.isArray(parsed)) {
        const candidate = (parsed as Record<string, unknown>).answers ?? parsed;
        if (candidate && typeof candidate === 'object' && !Array.isArray(candidate)) {
          const out: Record<string, string | string[]> = {};
          for (const [k, v] of Object.entries(candidate as Record<string, unknown>)) {
            if (typeof v === 'string') out[k] = v;
            else if (Array.isArray(v)) {
              const list = v.filter((entry): entry is string => typeof entry === 'string');
              if (list.length > 0) out[k] = list;
            }
          }
          return out;
        }
      }
    } catch {
      // Not JSON — fall through.
    }
    return {};
  }

  // Title rendering rule:
  // - Single question: "Question: <question text>" (truncates via CSS).
  // - Multi-question: "Question: N questions".
  const headerLabel = $derived.by<string>(() => {
    if (questions.length === 0) return 'Question';
    if (questions.length === 1) return `Question: ${questions[0].question}`;
    return `Question: ${questions.length} questions`;
  });

  const completionStatus = $derived(deriveCompletionStatus(item));
  const showRunningPill = $derived(item.status === 'running' || item.status === 'streaming');

  let time = $derived(
    new Date(item.createdAt).toLocaleTimeString(undefined, {
      hour: 'numeric',
      minute: '2-digit',
    }),
  );

  function answersForQuestion(q: AskQuestion): string[] {
    const id = q.id ?? '';
    const direct = id ? answersByQuestion[id] : undefined;
    if (direct !== undefined) return Array.isArray(direct) ? direct : [direct];
    // Some answers come back keyed by header or by question text. Try
    // those before giving up.
    const byHeader = q.header ? answersByQuestion[q.header] : undefined;
    if (byHeader !== undefined) return Array.isArray(byHeader) ? byHeader : [byHeader];
    const byQuestion = answersByQuestion[q.question];
    if (byQuestion !== undefined) return Array.isArray(byQuestion) ? byQuestion : [byQuestion];
    return [];
  }

  /**
   * Splits a question's answers into "matched a predefined option" vs
   * "custom typed answer." This lets the expanded body render
   * predefined options with check/X marks AND surface a separate
   * `Custom: <text>` row when the user picked "Other" and typed a
   * value.
   */
  function classifyAnswers(q: AskQuestion, answers: string[]) {
    const optionLabels = new Set((q.options ?? []).map((o) => o.label));
    const matched = new Set<string>();
    const customs: string[] = [];
    for (const answer of answers) {
      if (optionLabels.has(answer)) {
        matched.add(answer);
      } else if (answer.trim()) {
        customs.push(answer);
      }
    }
    return { matched, customs };
  }

</script>

<div
  class="group/tool mb-1.5 overflow-hidden"
  data-testid="ask-user-question-card"
  data-status={item.status}
>
  <button
    type="button"
    class="flex w-full items-center gap-2 rounded-[var(--radius-control)] px-1 py-1 text-left hover:bg-surface-2/20 cursor-pointer focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/40 transition-colors"
    onclick={toggle}
    onkeydown={handleKeydown}
    aria-expanded={expansion.expanded}
    aria-controls="ask-user-question-body-{item.id}"
    data-testid="ask-user-question-toggle"
  >
    <span
      class="flex size-3 shrink-0 items-center justify-center text-fg-subtle select-none transition-transform duration-150"
      class:rotate-90={expansion.expanded}
      aria-hidden="true"
    >
      <Icon icon={ChevronRight} size={12} strokeWidth={2} class="opacity-70" />
    </span>
    <span class="text-fg-muted shrink-0" aria-label="Question">
      <Icon icon={HelpCircle} size={14} />
    </span>
    <span
      class="text-[11px] font-medium text-fg-muted shrink-0 uppercase tracking-[0.04em]"
      data-testid="ask-user-question-label"
    >
      Ask
    </span>
    <span
      class="min-w-0 flex-1 truncate text-[12px] text-fg-muted/75"
      data-testid="ask-user-question-title"
    >
      {headerLabel}
    </span>
    {#if showRunningPill}
      <span
        class="shrink-0 text-[10px] text-accent opacity-70 transition-opacity group-hover/tool:opacity-100"
        data-testid="ask-user-question-status"
        data-status={item.status}
      >
        running
      </span>
    {/if}
    {#if !showRunningPill && completionStatus !== null}
      <CompletionBadge
        status={completionStatus}
        class="opacity-80 transition-opacity group-hover/tool:opacity-100"
      />
    {/if}
    <time
      class="shrink-0 tabular-nums text-[10px] text-fg-hint"
      datetime={new Date(item.createdAt).toISOString()}
      data-testid="ask-user-question-time"
    >
      {time}
    </time>
  </button>

  {#if expansion.expanded}
    <!-- No transition: the timeline scroll surface forbids height-shifting
         transitions adjacent to it (frontend/CLAUDE.md "Anti-patterns").
         The chevron rotation on the toggle is enough visual feedback. -->
    <div
      id="ask-user-question-body-{item.id}"
      class="ml-5 border-l border-border-subtle bg-surface-0/35 px-3 py-2"
      data-testid="ask-user-question-body"
    >
      {#if questions.length === 0}
        <p class="text-[11px] text-fg-subtle italic">
          No question metadata stored on this row.
        </p>
      {:else}
        <ul class="space-y-3">
          {#each questions as q, qIndex}
            {@const answers = answersForQuestion(q)}
            {@const { matched, customs } = classifyAnswers(q, answers)}
            <li class="space-y-1.5" data-testid="ask-user-question-question-{qIndex}">
              {#if q.header}
                <p class="text-[10px] font-semibold uppercase tracking-[0.06em] text-fg-muted">
                  {q.header}
                </p>
              {/if}
              <p class="text-xs text-fg">{q.question}</p>
              {#if q.options && q.options.length > 0}
                <ul class="ml-2 space-y-1">
                  {#each q.options as option (option.label)}
                    {@const isSelected = matched.has(option.label)}
                    <li
                      class="flex items-start gap-2 text-[11px]"
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
                          <p class="text-[10.5px] text-fg-subtle">{option.description}</p>
                        {/if}
                        {#if isSelected && option.preview?.trim()}
                          <div
                            class="mt-1 max-h-40 overflow-y-auto rounded border border-border-subtle bg-surface-0 px-2 py-1"
                            data-testid="ask-user-question-preview"
                          >
                            <ChatMarkdown source={option.preview} class="text-[11px]" />
                          </div>
                        {/if}
                      </div>
                    </li>
                  {/each}
                </ul>
              {/if}
              {#each customs as customAnswer}
                <div
                  class="ml-2 flex items-start gap-2 text-[11px]"
                  data-testid="ask-user-question-custom"
                >
                  <span class="mt-0.5 shrink-0 text-success" aria-label="Custom answer">
                    <Icon icon={Check} size={12} />
                  </span>
                  <p class="min-w-0 flex-1 text-fg">
                    <span class="text-[10.5px] font-semibold uppercase tracking-[0.06em] text-fg-muted">Custom:</span>
                    <span class="ml-1">{customAnswer}</span>
                  </p>
                </div>
              {/each}
              {#if answers.length === 0 && !showRunningPill}
                <p class="ml-2 text-[11px] italic text-fg-subtle">
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
