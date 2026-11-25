<script lang="ts">
  import X from '@lucide/svelte/icons/x';
  import Icon from '../primitives/Icon.svelte';
  import IconButton from '../primitives/IconButton.svelte';
  import ErrorReportActions from './ErrorReportActions.svelte';
  import ErrorReportDetails from './ErrorReportDetails.svelte';
  import type { CapturedError } from '../../stores/errorReports.svelte';

  // A failure of a tool page's own call (its header's refresh) as one line
  // under the header: the sentence, Details and copy through the shared
  // error report controls, and a dismiss unless the page has nothing else
  // to show in its place.

  interface Props {
    message: string;
    captured: CapturedError;
    /** None while the notice is all the page shows. */
    onDismiss?: () => void;
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
      {#if onDismiss}
        <IconButton label="Dismiss" size="xs" variant="tint" testId="{testId}-dismiss" onClick={onDismiss}>
          {#snippet children()}<Icon icon={X} size={12} strokeWidth={2.25} />{/snippet}
        </IconButton>
      {/if}
    </span>
  </div>
  {#if expanded}
    <ErrorReportDetails {captured} />
  {/if}
</div>
