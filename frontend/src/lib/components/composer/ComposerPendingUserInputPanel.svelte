<script lang="ts">
  import { onDestroy, onMount, untrack } from 'svelte';
  import Check from 'lucide-svelte/icons/check';
  import type { UserInputQuestion, UserInputRequest } from '../../types/events';
  import { UserInputResponse } from '../../stores/bindings';
  import { errString } from '../../utils/errors';
  import Icon from '../primitives/Icon.svelte';
  import Button from '../primitives/Button.svelte';
  import {
    createUserInputAnswers,
    firstUnansweredIndex,
    hasAnswer,
    isRequestComplete,
    selectedAnswers,
    setCustomAnswer,
    toResponseAnswers,
    toggleOptionAnswer,
    type UserInputAnswers,
  } from './pendingUserInput';

  interface Props {
    request: UserInputRequest;
    customAnswer: string;
    submitSignal: number;
    setCustomAnswerText: (value: string) => void;
    onResolve: (response: UserInputResponse) => Promise<void>;
    onResolved: () => void;
    onError: (message: string) => void;
  }

  let {
    request,
    customAnswer,
    submitSignal,
    setCustomAnswerText,
    onResolve,
    onResolved,
    onError,
  }: Props = $props();

  let index = $state(0);
  let answers: UserInputAnswers = $state(createUserInputAnswers());
  let customAnswers: Record<string, string> = $state(Object.create(null));
  let responding = $state(false);
  let handledSubmitSignal = $state(0);
  let autoAdvanceTimer: ReturnType<typeof setTimeout> | undefined;

  const question = $derived(request.questions[index]);
  const progressLabel = $derived(`${Math.min(index + 1, request.questions.length)}/${request.questions.length}`);
  const complete = $derived(isRequestComplete(request, answers));
  const canGoPrevious = $derived(index > 0 && !responding);
  const canGoNext = $derived(Boolean(question) && index < request.questions.length - 1 && hasAnswer(answers, question) && !responding);
  const canSubmit = $derived(complete && !responding);

  $effect(() => {
    if (!question) return;
    const value = customAnswer;
    const activeQuestion = question;
    const previousCustomAnswer = untrack(() => customAnswers[activeQuestion.id] ?? '');
    if (previousCustomAnswer === value) return;

    customAnswers = Object.assign(Object.create(null), untrack(() => customAnswers), {
      [activeQuestion.id]: value,
    });
    if (!value.trim() && !previousCustomAnswer.trim()) return;
    answers = setCustomAnswer(untrack(() => answers), activeQuestion, value);
  });

  $effect(() => {
    const signal = submitSignal;
    if (signal === handledSubmitSignal) return;
    handledSubmitSignal = signal;
    if (signal > 0) {
      void advanceOrSubmit();
    }
  });

  function clearAutoAdvanceTimer(): void {
    if (!autoAdvanceTimer) return;
    clearTimeout(autoAdvanceTimer);
    autoAdvanceTimer = undefined;
  }

  function showQuestion(nextIndex: number): void {
    clearAutoAdvanceTimer();
    index = Math.min(Math.max(nextIndex, 0), request.questions.length - 1);
    const nextQuestion = request.questions[index];
    setCustomAnswerText(nextQuestion ? customAnswers[nextQuestion.id] ?? '' : '');
  }

  function selectOption(activeQuestion: UserInputQuestion, label: string): void {
    clearAutoAdvanceTimer();
    customAnswers = Object.assign(Object.create(null), customAnswers, { [activeQuestion.id]: '' });
    setCustomAnswerText('');
    const nextAnswers = toggleOptionAnswer(answers, activeQuestion, label);
    answers = nextAnswers;
    if (!activeQuestion.multiSelect) {
      autoAdvanceTimer = setTimeout(() => {
        if (index < request.questions.length - 1) {
          showQuestion(index + 1);
          return;
        }
        if (isRequestComplete(request, nextAnswers)) {
          void submit(nextAnswers);
        }
      }, 200);
    }
  }

  async function submit(answersToSubmit: UserInputAnswers = answers): Promise<void> {
    if (!isRequestComplete(request, answersToSubmit) || responding) return;
    responding = true;
    try {
      await onResolve(new UserInputResponse({
        requestId: request.requestId,
        decision: 'accept',
        answers: toResponseAnswers(answersToSubmit),
      }));
      setCustomAnswerText('');
      onResolved();
    } catch (err) {
      console.error('Failed to submit user input:', err);
      onError(`Failed to submit input: ${errString(err)}`);
      responding = false;
    }
  }

  async function advanceOrSubmit(): Promise<void> {
    if (!question || responding || !hasAnswer(answers, question)) return;
    if (index < request.questions.length - 1) {
      showQuestion(index + 1);
      return;
    }
    await submit();
  }

  function handleWindowKeydown(event: KeyboardEvent): void {
    if (!question || responding) return;
    if (event.metaKey || event.ctrlKey || event.altKey) return;
    const target = event.target as HTMLElement | null;
    if (target?.matches('textarea, input, select, [contenteditable="true"]')) return;
    if (target?.closest('[contenteditable]:not([contenteditable="false"])')) return;
    const optionIndex = Number.parseInt(event.key, 10) - 1;
    if (!Number.isInteger(optionIndex) || optionIndex < 0) return;
    const option = question.options?.[optionIndex];
    if (!option) return;
    event.preventDefault();
    selectOption(question, option.label);
  }

  onMount(() => {
    const first = firstUnansweredIndex(request, answers);
    index = first;
    window.addEventListener('keydown', handleWindowKeydown);
  });

  onDestroy(() => {
    clearAutoAdvanceTimer();
    window.removeEventListener('keydown', handleWindowKeydown);
  });
</script>

<section
  class="border-b border-border-subtle bg-surface-1/70 px-4 py-3"
  data-testid="composer-pending-user-input"
  aria-live="polite"
>
  <div class="flex items-start justify-between gap-3">
    <div class="min-w-0">
      <div class="flex items-center gap-2">
        <p class="text-[10px] font-semibold uppercase tracking-[0.08em] text-fg-muted">
          Input requested
        </p>
        {#if request.questions.length > 1}
          <span class="rounded-full border border-border-subtle px-1.5 py-0.5 text-[10px] text-fg-muted">
            {progressLabel}
          </span>
        {/if}
      </div>
      {#if question}
        <p class="mt-1 text-[13px] font-medium text-fg">{question.header || request.title}</p>
        <p class="mt-0.5 text-xs text-fg-muted">{question.question}</p>
      {/if}
    </div>
  </div>

  {#if question?.options?.length}
    <div class="mt-3 grid gap-1.5" data-testid="user-input-options">
      {#each question.options as option, optionIndex (option.label)}
        {@const selected = selectedAnswers(answers, question).includes(option.label)}
        <button
          type="button"
          class={[
            'flex w-full items-start gap-2 rounded-[var(--radius-control)] border px-2.5 py-2 text-left',
            'transition-[background-color,border-color,color] duration-150',
            'focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/40',
            selected
              ? 'border-accent/50 bg-accent/10 text-fg'
              : 'border-border-subtle bg-surface-0/40 text-fg-muted hover:border-border hover:text-fg',
          ].join(' ')}
          data-testid={`user-input-option-${optionIndex + 1}`}
          aria-pressed={selected ? 'true' : 'false'}
          disabled={responding}
          onclick={() => selectOption(question, option.label)}
        >
          <span class="mt-0.5 inline-flex h-4 min-w-4 items-center justify-center rounded border border-border-subtle text-[10px] text-fg-muted">
            {optionIndex + 1}
          </span>
          <span class="min-w-0 flex-1">
            <span class="block text-xs font-medium">{option.label}</span>
            {#if option.description}
              <span class="mt-0.5 block text-[11px] leading-4 text-fg-muted">{option.description}</span>
            {/if}
          </span>
          {#if selected}
            <Icon icon={Check} size={14} class="mt-0.5 text-accent" />
          {/if}
        </button>
      {/each}
    </div>
  {/if}

  <div class="mt-3 flex flex-wrap justify-end gap-2">
    <Button variant="secondary" size="sm" onclick={() => showQuestion(index - 1)} testId="user-input-previous" disabled={!canGoPrevious}>
      {#snippet children()}Previous{/snippet}
    </Button>
    {#if index < request.questions.length - 1}
      <Button variant="primary" size="sm" onclick={advanceOrSubmit} testId="user-input-next" disabled={!canGoNext}>
        {#snippet children()}Next question{/snippet}
      </Button>
    {:else}
      <Button variant="primary" size="sm" onclick={() => submit()} testId="user-input-submit" disabled={!canSubmit} loading={responding}>
        {#snippet children()}Submit answer(s){/snippet}
      </Button>
    {/if}
  </div>
</section>
