<script lang="ts">
  // The one render boundary. A throw during an update flush (a keyed-each
  // collision, a nullable read on a row that is not there yet) is otherwise
  // uncaught: svelte aborts the whole batch and every region the traversal
  // had not reached keeps its stale DOM for good — a composer that will not
  // clear, a reveal that stops mid-message (incidents 2026-08-29 and
  // 2026-09-04). Inside a boundary the throw tears down only this subtree,
  // which renders the failure in place with a Retry, and the rest of the
  // page keeps updating.
  //
  // The boundary swallows the throw before `window.onerror` sees it, so the
  // record that used to land in frontend-errors.jsonl is written here
  // instead: the failure is visible on screen AND on disk.
  import type { Snippet } from 'svelte';
  import { reportFrontendDiagnostic } from '../../utils/frontendErrorCapture';

  interface Props {
    /**
     * What the boundary guards, as the failure row and the log name it:
     * "The review pane", "This thread pane", "The thread list".
     */
    label: string;
    /** `data-testid` of the failure row; its Retry button is `${testId}-retry`. */
    testId: string;
    children: Snippet;
  }

  let { label, testId, children }: Props = $props();

  function failureMessage(error: unknown): string {
    return error instanceof Error ? error.message : String(error);
  }

  function report(error: unknown): void {
    reportFrontendDiagnostic(
      `${label} failed to render: ${failureMessage(error)}`,
      error instanceof Error ? (error.stack ?? '') : '',
    );
  }
</script>

<svelte:boundary onerror={report}>
  {@render children()}
  {#snippet failed(error, reset)}
    <div
      class="flex flex-col gap-2 border-b border-error/30 bg-error/10 px-3 py-2 text-xs text-error"
      role="alert"
      data-testid={testId}
    >
      <div>{label} failed to render: {failureMessage(error)}</div>
      <div>
        <button
          type="button"
          class="rounded border border-error/45 px-2 py-1 text-[0.6875rem] font-medium hover:bg-error/10"
          data-testid="{testId}-retry"
          onclick={reset}
        >
          Retry
        </button>
      </div>
    </div>
  {/snippet}
</svelte:boundary>
