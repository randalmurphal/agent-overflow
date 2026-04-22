<script lang="ts">
  import type { ApprovalRequest } from '../../../types/events';
  import { ApprovalResponse } from '../../../stores/bindings';
  import { errString } from '../../../utils/errors';
  import Button from '../../primitives/Button.svelte';

  interface Props {
    approval: ApprovalRequest;
    onResolve: (response: ApprovalResponse) => Promise<void>;
    onError: (message: string) => void;
  }

  let { approval, onResolve, onError }: Props = $props();

  // Per-question answer map keyed by question id. Fresh per approval
  // instance — the dispatcher re-mounts this component per requestId.
  let answers: Record<string, string> = $state({});

  function setAnswer(questionId: string, value: string) {
    answers = { ...answers, [questionId]: value };
  }

  async function submit() {
    try {
      await onResolve(new ApprovalResponse({
        requestId: approval.requestId,
        decision: 'allow',
        answers,
      }));
    } catch (err) {
      console.error('Failed to submit user input:', err);
      onError(`Failed to submit input: ${errString(err)}`);
    }
  }

  async function cancel() {
    try {
      await onResolve(new ApprovalResponse({
        requestId: approval.requestId,
        decision: 'deny',
      }));
    } catch (err) {
      console.error('Failed to cancel user input:', err);
      onError(`Failed to respond to approval: ${errString(err)}`);
    }
  }
</script>

{#if approval.questions?.length}
  <div class="mt-2 space-y-2" data-testid="user-input-questions">
    {#each approval.questions as question (question.id)}
      <div>
        {#if question.header}
          <p class="text-xs font-medium text-text-primary">{question.header}</p>
        {/if}
        <label class="text-xs text-text-secondary block mt-0.5" for="q-{approval.requestId}-{question.id}">
          {question.question}
        </label>
        {#if question.options?.length}
          <select
            id="q-{approval.requestId}-{question.id}"
            data-testid="user-input-select-{question.id}"
            class="mt-1 w-full text-xs rounded border border-border bg-surface-1 px-2 py-1.5 text-text-primary"
            value={answers[question.id] ?? ''}
            onchange={(e) => setAnswer(question.id, (e.target as HTMLSelectElement).value)}
          >
            <option value="">Select...</option>
            {#each question.options as opt}
              <option value={opt.label}>{opt.label}{opt.description ? ` — ${opt.description}` : ''}</option>
            {/each}
          </select>
        {:else}
          <input
            id="q-{approval.requestId}-{question.id}"
            type="text"
            data-testid="user-input-text-{question.id}"
            class="mt-1 w-full text-xs rounded border border-border bg-surface-1 px-2 py-1.5 text-text-primary"
            value={answers[question.id] ?? ''}
            oninput={(e) => setAnswer(question.id, (e.target as HTMLInputElement).value)}
            placeholder="Enter response..."
          />
        {/if}
      </div>
    {/each}
  </div>
{/if}

<div class="flex gap-2 mt-2.5 justify-end">
  <Button variant="primary" size="sm" onclick={submit} testId="user-input-submit">
    {#snippet children()}Submit{/snippet}
  </Button>
  <Button variant="danger-outline" size="sm" onclick={cancel} testId="user-input-cancel">
    {#snippet children()}Cancel{/snippet}
  </Button>
</div>
