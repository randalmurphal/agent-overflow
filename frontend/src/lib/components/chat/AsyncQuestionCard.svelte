<script lang="ts">
  import { attachAsyncQuestions, dismissAsyncQuestion, type AsyncQuestion } from '../../stores/asyncQuestions.svelte';
  import type { EntityAttachment } from '../../stores/entityStore.svelte';
  import type { Item } from '../../types/models';
  import { parseJsonObject } from '../../utils/parseJsonObject';
  import { errString } from '../../utils/errors';
  import { threadHasScope } from '../../transport/entityScopes';
  import ChatMarkdown from './ChatMarkdown.svelte';
  import Button from '../primitives/Button.svelte';

  let { item, workspacePath = '' }: { item: Item; workspacePath?: string } = $props();
  let resource = $state<EntityAttachment<AsyncQuestion[]> | null>(null);
  let error = $state('');
  let questions = $derived((parseJsonObject(item.meta)?.questions ?? []) as { title: string; options?: string[] }[]);
  $effect(() => {
    if (!threadHasScope('threads:read', item.threadId)) return;
    const attachment = attachAsyncQuestions(item.threadId, item.id);
    resource = attachment;
    return attachment.release;
  });
  async function reopen(index: number) {
    try { await dismissAsyncQuestion(item.threadId, { itemId: item.id, index }, false); } catch (cause) { error = errString(cause); }
  }
</script>

<div class="mb-4 rounded-[var(--radius-control)] border border-border bg-surface-1 p-3" data-testid="async-question-card">
  <div class="mb-2 text-xs font-medium text-fg-muted">Questions</div>
  {#each questions as question, index (index)}
    {@const record = resource?.current?.find(q => q.index === index)}
    <div class="mb-2">
      <ChatMarkdown source={question.title} {workspacePath} />
      {#if question.options?.length}<ul class="mt-1 list-inside list-disc text-xs text-fg-muted">{#each question.options as option, i (i)}<li>{option}</li>{/each}</ul>{/if}
      {#if record?.answer}<p class="mt-1 text-sm text-fg">Answer: {record.answer}</p>{/if}
      <div class="mt-1 text-xs text-fg-muted">{record?.state === 'delivered' ? 'Answer delivered' : record?.state === 'submitted' ? 'Answer submitted' : record?.state === 'restored' ? 'Answer restored to composer' : record?.state === 'unanswered' ? 'Awaiting your answer' : record?.state === 'dismissed' ? 'Dismissed' : 'Historical question'}</div>
      {#if (!record || record.state === 'dismissed') && threadHasScope('threads:operate', item.threadId)}
        <Button size="sm" onclick={() => reopen(index)}>Open question</Button>
      {/if}
    </div>
  {/each}
  {#if error || resource?.error}<p role="alert" class="text-xs text-error">{error || resource?.error}</p>{/if}
</div>
