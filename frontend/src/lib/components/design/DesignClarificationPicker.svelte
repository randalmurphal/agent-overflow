<script lang="ts">
  // DesignClarificationPicker — when `pane.pendingClarification` is
  // non-null, render the agent's clarification questions as cards. On
  // submit, send a structured user message via SendMessage; the agent's
  // prompt teaches it to read the answers from the user's reply.
  //
  // `pane.pendingClarification` is populated by
  // `parseDesignAssistantPayloads` (see
  // `frontend/src/lib/utils/designAssistantPayload.ts`) which scans
  // assistant text for fenced `aoflow-design` blocks tagged with
  // `kind: "clarification_request"` and projects them onto pane state.
  //
  // Selection model: each question has a `multiple` flag. Multiple-choice
  // questions allow zero-or-more chips selected; single-choice (the
  // default) holds at most one. We require at least one answer per
  // question before enabling Submit so the agent isn't left guessing
  // about an unanswered question.

  import type { ThreadPane } from '../../stores/thread.svelte';
  import type {
    ClarificationQuestion,
    ClarificationRequest,
  } from '../../types/design';
  import { SendMessage } from '../../stores/bindings';
  import { addToast } from '../../stores/toast.svelte';

  let { pane }: { pane: ThreadPane } = $props();

  // Selections keyed by question id → selected choice ids. We keep the
  // map even for unrendered questions so a multi-question request can
  // round-trip individual choice changes without re-allocating the map
  // on each click.
  let selections = $state<Record<string, string[]>>({});
  let submitting = $state(false);

  // Reset the selections when the request id changes — a stale answer
  // for a previous request would otherwise leak into the new picker.
  let lastRequestId: string | null = null;
  $effect(() => {
    const request = pane.pendingClarification;
    const id = request?.requestId ?? null;
    if (id !== lastRequestId) {
      selections = {};
      lastRequestId = id;
    }
  });

  function isAnswered(question: ClarificationQuestion): boolean {
    const picks = selections[question.id];
    return Array.isArray(picks) && picks.length > 0;
  }

  function allAnswered(request: ClarificationRequest): boolean {
    return request.questions.every(isAnswered);
  }

  function toggleChoice(question: ClarificationQuestion, choiceId: string): void {
    const current = selections[question.id] ?? [];
    if (question.multiple) {
      const has = current.includes(choiceId);
      const next = has
        ? current.filter((id) => id !== choiceId)
        : [...current, choiceId];
      selections = { ...selections, [question.id]: next };
    } else {
      selections = { ...selections, [question.id]: [choiceId] };
    }
  }

  function isSelected(questionId: string, choiceId: string): boolean {
    const picks = selections[questionId];
    return Array.isArray(picks) && picks.includes(choiceId);
  }

  function buildAnswers(request: ClarificationRequest): Array<{
    questionId: string;
    choiceIds: string[];
  }> {
    return request.questions.map((q) => ({
      questionId: q.id,
      choiceIds: selections[q.id] ?? [],
    }));
  }

  async function submit(request: ClarificationRequest): Promise<void> {
    const threadId = pane.threadId;
    if (!threadId || submitting || !allAnswered(request)) return;
    submitting = true;
    try {
      const json = JSON.stringify(
        {
          kind: 'clarification_response',
          requestId: request.requestId,
          answers: buildAnswers(request),
        },
        null,
        2,
      );
      const body = `Clarification answers:\n\n\`\`\`aoflow-design\n${json}\n\`\`\``;
      await SendMessage(threadId, body, []);
      pane.setPendingClarification(null);
    } catch (err) {
      const m = err instanceof Error ? err.message : String(err);
      addToast('error', `Failed to send answers: ${m}`);
    } finally {
      submitting = false;
    }
  }
</script>

{#if pane.pendingClarification}
  {@const request = pane.pendingClarification}
  <section
    class="flex flex-col min-h-0 bg-transparent border-t border-border-subtle"
    data-testid="design-clarification-picker"
  >
    <div class="flex items-baseline justify-between gap-2 px-3 pt-3 pb-2 border-b border-border-subtle shrink-0">
      <div>
        <p class="text-[11px] font-semibold uppercase tracking-[0.18em] text-fg-subtle">
          Clarification
        </p>
        {#if request.intro}
          <p class="text-[12px] text-fg-muted mt-1 leading-relaxed">{request.intro}</p>
        {/if}
      </div>
      <span class="text-[10px] text-fg-hint font-mono shrink-0" title="Request ID">
        req {request.requestId.slice(0, 8)}
      </span>
    </div>
    <div class="flex-1 min-h-0 overflow-y-auto px-3 py-2 space-y-3">
      {#each request.questions as question (question.id)}
        <fieldset class="flex flex-col gap-1.5">
          <legend class="text-[12px] font-medium text-fg">
            {question.prompt}
            {#if question.multiple}
              <span class="text-[10px] text-fg-hint ml-1">(pick any)</span>
            {/if}
          </legend>
          <div class="flex flex-wrap gap-2">
            {#each question.choices as choice (choice.id)}
              {@const selected = isSelected(question.id, choice.id)}
              <button
                type="button"
                aria-pressed={selected}
                onclick={() => toggleChoice(question, choice.id)}
                disabled={submitting}
                class={[
                  'flex-1 min-w-[140px] text-left rounded-[var(--radius-control)]',
                  'border px-3 py-2 transition-colors cursor-pointer',
                  'focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/40',
                  'disabled:cursor-not-allowed',
                  selected
                    ? 'border-accent/70 bg-accent/10 text-fg'
                    : 'border-border-subtle bg-card/30 text-fg-muted hover:bg-surface-2/30 hover:text-fg',
                ].join(' ')}
                data-testid="design-clarification-choice"
              >
                <span class="text-[12px]">{choice.label}</span>
              </button>
            {/each}
          </div>
        </fieldset>
      {/each}
    </div>
    <div class="flex items-center justify-end gap-2 border-t border-border-subtle px-3 py-2 shrink-0">
      <button
        type="button"
        onclick={() => void submit(request)}
        disabled={submitting || !allAnswered(request)}
        class={[
          'inline-flex items-center gap-1 rounded-[var(--radius-field)]',
          'border border-accent/60 bg-accent/15 px-3 py-1',
          'text-[12px] text-fg cursor-pointer transition-colors',
          'hover:bg-accent/25',
          'focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/40',
          'disabled:opacity-50 disabled:cursor-not-allowed',
        ].join(' ')}
        data-testid="design-clarification-submit"
      >
        {submitting ? 'Sending…' : 'Submit answers'}
      </button>
    </div>
  </section>
{/if}
