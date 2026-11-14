<script lang="ts">
  import X from '@lucide/svelte/icons/x';
  import Icon from '../primitives/Icon.svelte';
  import IconButton from '../primitives/IconButton.svelte';
  import ErrorReportActions from '../shared/ErrorReportActions.svelte';
  import ErrorReportDetails from '../shared/ErrorReportDetails.svelte';
  import type { CapturedError } from '../../stores/errorReports.svelte';

  // A failure of the page's own call (the header's refresh) as one line
  // under the header: the sentence, Details and copy through the shared
  // error report controls, and a dismiss.

  interface Props {
    message: string;
    captured: CapturedError;
    onDismiss: () => void;
    testId: string;
  }

  let { message, captured, onDismiss, testId }: Props = $props();
  let expanded = $state(false);
</script>

<div class="border-b border-error/30 bg-error/10 px-3 py-1.5 text-xs text-error" role="alert" data-testid={testId}>
  <div class="flex items-center gap-2">
    <span class="min-w-0 flex-1 break-words">{message}</span>
    <span class="flex shrink-0 items-center gap-0.5">
      <ErrorReportActions {captured} {expanded} onToggle={() => { expanded = !expanded; }} />
      <IconButton label="Dismiss" size="xs" variant="tint" testId="{testId}-dismiss" onClick={onDismiss}>
        {#snippet children()}<Icon icon={X} size={12} strokeWidth={2.25} />{/snippet}
      </IconButton>
    </span>
  </div>
  {#if expanded}
    <ErrorReportDetails {captured} />
  {/if}
</div>
