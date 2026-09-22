<script lang="ts">
  import { threadBackend } from '../../transport/entityIndex';
  import { untrack } from 'svelte';
  import { attachAsyncQuestions, attachAsyncQuestionDraft, dismissAsyncQuestion, questionKey, type AsyncQuestion, type AsyncQuestionDraft } from '../../stores/asyncQuestions.svelte';
  import type { EntityAttachment } from '../../stores/entityStore.svelte';
  import { threadHasScope } from '../../transport/entityScopes';
  import { errString } from '../../utils/errors';
  import ChatMarkdown from '../chat/ChatMarkdown.svelte';
  import Button from '../primitives/Button.svelte';
  import UserInputOptionButton from './UserInputOptionButton.svelte';

  let { threadId, workspacePath = '' }: { threadId: string; workspacePath?: string } = $props();
  let resource = $state<EntityAttachment<AsyncQuestion[]> | null>(null);
  let draft = $state<AsyncQuestionDraft | null>(null);
  let error = $state('');
  let collapsed = $state(false);
  let focusedOption = $state(-1);
  $effect(() => {
    const id = threadId;
    resource = null; draft = null;
    if (!threadHasScope('threads:read', id) || threadBackend(id) === undefined) return;
    const attachment = attachAsyncQuestions(id);
    resource = attachment;
    let releaseDraft: (() => void) | undefined;
    untrack(() => {
      try {
        const held = attachAsyncQuestionDraft(id);
        draft = held.draft; releaseDraft = held.release; error = '';
      } catch (cause) { error = errString(cause); }
    });
    return () => { attachment.release(); releaseDraft?.(); };
  });
  let rows = $derived(resource?.current ?? []);
  let unanswered = $derived(rows.filter(q => q.state === 'unanswered'));
  let active = $derived(unanswered.find(q => questionKey(q) === draft?.active) ?? unanswered[0]);
  let activeIndex = $derived(active ? unanswered.indexOf(active) : -1);
  let answer = $derived(active && draft ? draft.answers[questionKey(active)] ?? '' : '');
  let answeredCount = $derived(unanswered.filter(q => draft?.answers[questionKey(q)]?.trim()).length);
  let permitted = $derived(threadHasScope('threads:operate', threadId));
  let disabled = $derived(!permitted || !draft || (active ? draft.isPending(active) : false));
  function change(value: string) {
    if (!active || !draft) return;
    try { draft.answer(questionKey(active), value); error = ''; } catch (cause) { error = errString(cause); }
  }
  function navigate(index: number) {
    const next = unanswered[index];
    if (!next || !draft) return;
    try { draft.focus(questionKey(next)); focusedOption = -1; } catch (cause) { error = errString(cause); }
  }
  async function submit() {
    if (!draft || !permitted) return;
    error = '';
    try { await draft.submit(unanswered); } catch (cause) { error = errString(cause); }
  }
  async function dismiss() {
    if (!active || !permitted) return;
    try { await dismissAsyncQuestion(threadId, active, true); } catch (cause) { error = errString(cause); }
  }
</script>

{#if rows.length || error || resource?.error || draft?.retrying}
  <section class="mb-2 rounded-[var(--radius-control)] border border-border bg-surface-1 p-3" data-testid="async-question-picker" aria-label="Agent questions">
    <div class="flex items-center justify-between gap-2">
      <button type="button" class="text-xs font-medium text-fg" onclick={() => collapsed = !collapsed} aria-expanded={!collapsed}>
        Questions{unanswered.length ? ` · ${unanswered.length} unanswered` : ''}
      </button>
      {#if active && !collapsed}<span class="text-xs text-fg-muted" data-testid="async-question-position">{activeIndex + 1} / {unanswered.length}</span>{/if}
    </div>
    {#if !collapsed}
      {#if active}
        <div class="my-2 max-h-64 overflow-y-auto" data-testid="async-question-body">
          <ChatMarkdown source={active.title} {workspacePath} />
          <div class="mt-2 flex flex-col gap-1">
            {#each active.options ?? [] as option, i (i)}
              <UserInputOptionButton label={option} description="" optionIndex={i} selected={answer === option} focused={focusedOption === i} {disabled} tabIndex={0} onSelect={() => change(option)} onFocus={() => focusedOption = i} />
            {/each}
          </div>
          <textarea class="mt-2 w-full resize-y rounded-[var(--radius-control)] border border-border bg-surface-0 p-2 text-sm text-fg" aria-label="Your answer" placeholder="Type your answer" value={answer} {disabled} oninput={event => change(event.currentTarget.value)} rows={2}></textarea>
        </div>
        <div class="flex flex-wrap items-center gap-2">
          <Button size="sm" disabled={activeIndex <= 0} onclick={() => navigate(activeIndex - 1)}>Previous</Button>
          <Button size="sm" disabled={activeIndex >= unanswered.length - 1} onclick={() => navigate(activeIndex + 1)}>Next</Button>
          <Button size="sm" {disabled} onclick={dismiss}>Dismiss question</Button>
          <Button size="sm" disabled={!permitted || !draft || draft.sending || (!answeredCount && !draft.retrying)} onclick={submit}>
            {draft?.retrying ? 'Retry sending answers' : `Send answered (${answeredCount})`}
          </Button>
        </div>
      {:else if draft?.retrying}
        <Button size="sm" disabled={!permitted || draft.sending} onclick={submit}>Retry sending answers</Button>
      {/if}
      {#if rows.some(q => q.state === 'submitted')}<p class="mt-2 text-xs text-fg-muted">Answers submitted; waiting for delivery.</p>{/if}
      {#if rows.some(q => q.state === 'restored')}<p class="mt-2 text-xs text-fg-muted">Unsent answers were restored to the message composer.</p>{/if}
    {/if}
    {#if error || resource?.error}<p role="alert" class="mt-2 text-xs text-error">{error || resource?.error}</p>{/if}
  </section>
{/if}
