<script lang="ts">
  import ErrorReportActions from '../shared/ErrorReportActions.svelte';
  import ErrorReportDetails from '../shared/ErrorReportDetails.svelte';
  import type { PRManageError } from '../../stores/prManage.svelte';

  // A management write's failure on the editor that caused it: the
  // backend's one sentence, with Details and copy through the shared
  // error report controls.

  interface Props {
    error: PRManageError;
    testid: string;
  }

  let { error, testid }: Props = $props();
  let expanded = $state(false);
</script>

<div class="rounded-[var(--radius-field)] bg-error/10 px-2 py-1 text-[0.6875rem] text-error" role="alert" data-testid={testid}>
  <div class="flex items-start gap-1.5">
    <span class="min-w-0 flex-1 break-words py-0.5" data-testid="{testid}-message">{error.message}</span>
    <span class="flex shrink-0 items-center gap-0.5">
      <ErrorReportActions captured={error.captured} {expanded} onToggle={() => { expanded = !expanded; }} />
    </span>
  </div>
  {#if expanded}
    <ErrorReportDetails captured={error.captured} />
  {/if}
</div>
