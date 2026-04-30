<script lang="ts">
  import { onDestroy, onMount, untrack } from 'svelte';
  import { scale } from 'svelte/transition';
  import type { UserInputQuestion, UserInputRequest } from '../../types/events';
  import { UserInputResponse } from '../../stores/bindings';
  import { errString } from '../../utils/errors';
  import Button from '../primitives/Button.svelte';
  import ChatMarkdown from '../chat/ChatMarkdown.svelte';
  import UserInputOptionButton from './UserInputOptionButton.svelte';
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
  let focusedOptionIndex = $state(0);
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

  // Side-by-side preview pane is single-select only per the upstream tool
  // spec. A multi-select question with previews still renders the option
  // list in a single column — the preview field is silently ignored.
  const hasPreviews = $derived(
    !!question && !question.multiSelect && (question.options?.some((option) => option.preview?.trim()) ?? false),
  );
  const focusedPreview = $derived(
    hasPreviews ? (question?.options?.[focusedOptionIndex]?.preview ?? '') : '',
  );

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
    focusedOptionIndex = 0;
    const nextQuestion = request.questions[index];
    setCustomAnswerText(nextQuestion ? customAnswers[nextQuestion.id] ?? '' : '');
  }

  /**
   * Selects (or toggles) an option for the active question.
   *
   * `originHint` controls whether selection auto-advances:
   * - `'keyboard'` (number key 1-9, fast path): single-select selection
   *   schedules the 200ms auto-advance/auto-submit that keypress users
   *   expect.
   * - `'mouse'` (click): selection only — the user must click "Next
   *   question" or "Submit answer(s)" to proceed. This is the fix for
   *   the "I clicked an option and the dialog jumped on me" issue;
   *   users need a chance to review their selection or change their
   *   mind before committing.
   *
   * Multi-select questions never auto-advance regardless of origin; the
   * user always uses the explicit submit/next button.
   */
  function selectOption(
    activeQuestion: UserInputQuestion,
    label: string,
    originHint: 'mouse' | 'keyboard',
  ): void {
    clearAutoAdvanceTimer();
    customAnswers = Object.assign(Object.create(null), customAnswers, { [activeQuestion.id]: '' });
    setCustomAnswerText('');
    const nextAnswers = toggleOptionAnswer(answers, activeQuestion, label);
    answers = nextAnswers;
    const optionIndex = activeQuestion.options?.findIndex((option) => option.label === label) ?? -1;
    if (optionIndex >= 0) {
      focusedOptionIndex = optionIndex;
    }
    if (activeQuestion.multiSelect || originHint !== 'keyboard') {
      return;
    }
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
    // event.target is `EventTarget`, not always `Element` (window-level
    // dispatch in test environments leaves it as the Window object).
    // Guard with `instanceof Element` before calling matches/closest so
    // the focused-input bypass works in production without exploding in
    // happy-dom.
    const targetEl = event.target instanceof Element ? event.target : null;
    if (targetEl?.matches('textarea, input, select, [contenteditable="true"]')) return;
    if (targetEl?.closest('[contenteditable]:not([contenteditable="false"])')) return;

    const optionCount = question.options?.length ?? 0;

    // Arrow Up/Down moves the focused option (drives the side-by-side
    // preview pane) without selecting it. Clamp to list boundaries; no
    // wrap-around so the user gets a clear edge feel.
    if (event.key === 'ArrowDown' && optionCount > 0) {
      event.preventDefault();
      focusedOptionIndex = Math.min(focusedOptionIndex + 1, optionCount - 1);
      return;
    }
    if (event.key === 'ArrowUp' && optionCount > 0) {
      event.preventDefault();
      focusedOptionIndex = Math.max(focusedOptionIndex - 1, 0);
      return;
    }

    const optionIndex = Number.parseInt(event.key, 10) - 1;
    if (!Number.isInteger(optionIndex) || optionIndex < 0) return;
    const option = question.options?.[optionIndex];
    if (!option) return;
    event.preventDefault();
    selectOption(question, option.label, 'keyboard');
  }

  onMount(() => {
    const first = firstUnansweredIndex(request, answers);
    index = first;
    focusedOptionIndex = 0;
    window.addEventListener('keydown', handleWindowKeydown);
  });

  onDestroy(() => {
    clearAutoAdvanceTimer();
    window.removeEventListener('keydown', handleWindowKeydown);
  });
</script>

<section
  class="border-b-2 border-accent/60 px-4 py-4 shadow-[inset_0_2px_0_oklch(from_var(--accent)_l_c_h/0.18)]"
  data-testid="composer-pending-user-input"
  aria-live="assertive"
  in:scale={{ duration: 200, start: 0.96, opacity: 0 }}
>
  <div class="flex items-start justify-between gap-3">
    <div class="min-w-0">
      <div class="flex items-center gap-2">
        <p class="text-[11px] font-bold uppercase tracking-[0.08em] text-accent">
          Input requested
        </p>
        {#if request.questions.length > 1}
          <span class="rounded-full border border-accent/40 bg-accent/10 px-1.5 py-0.5 text-[10px] font-semibold text-accent">
            {progressLabel}
          </span>
        {/if}
      </div>
      {#if question}
        <p class="mt-1 text-sm font-semibold text-fg">{question.header || request.title}</p>
        <p class="mt-0.5 text-xs text-fg-muted">{question.question}</p>
      {/if}
    </div>
  </div>

  {#if question?.options?.length}
    {#if hasPreviews}
      <!-- Side-by-side: option list left, focused-option preview right.
           Triggered when at least one option in a single-select question
           carries a non-empty `preview` field. Multi-select questions
           ignore previews per the upstream tool spec. -->
      <div
        class="mt-3 grid grid-cols-[minmax(0,2fr)_minmax(0,3fr)] gap-3"
        data-testid="user-input-options"
      >
        <div class="grid gap-1.5">
          {#each question.options as option, optionIndex (option.label)}
            {@const selected = selectedAnswers(answers, question).includes(option.label)}
            <UserInputOptionButton
              label={option.label}
              description={option.description}
              {optionIndex}
              {selected}
              focused={focusedOptionIndex === optionIndex}
              disabled={responding}
              onSelect={() => selectOption(question, option.label, 'mouse')}
              onFocus={() => (focusedOptionIndex = optionIndex)}
            />
          {/each}
        </div>
        <div
          class="max-h-60 overflow-y-auto rounded border border-border-subtle bg-surface-0 px-2.5 py-1.5"
          data-testid="user-input-preview"
        >
          {#if focusedPreview.trim()}
            <ChatMarkdown source={focusedPreview} class="text-xs" />
          {:else}
            <p class="text-xs text-fg-muted italic">No preview for this option.</p>
          {/if}
        </div>
      </div>
    {:else}
      <div class="mt-3 grid gap-1.5" data-testid="user-input-options">
        {#each question.options as option, optionIndex (option.label)}
          {@const selected = selectedAnswers(answers, question).includes(option.label)}
          <UserInputOptionButton
            label={option.label}
            description={option.description}
            {optionIndex}
            {selected}
            focused={focusedOptionIndex === optionIndex}
            disabled={responding}
            onSelect={() => selectOption(question, option.label, 'mouse')}
            onFocus={() => (focusedOptionIndex = optionIndex)}
          />
        {/each}
      </div>
    {/if}
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
