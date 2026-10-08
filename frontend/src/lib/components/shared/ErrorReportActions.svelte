<script lang="ts">
  // The icon controls an error notice carries beside its dismiss: expand the
  // details, and copy the whole report for an agent. The notice owns the
  // expanded state and renders ErrorReportDetails under its message.
  import ChevronDown from '@lucide/svelte/icons/chevron-down';
  import Icon from '../primitives/Icon.svelte';
  import IconButton from '../primitives/IconButton.svelte';
  import CopyButton from '../primitives/CopyButton.svelte';
  import { capturedErrorText, type CapturedError } from '../../stores/errorReports.svelte';
  import { errorHasDetails } from '../../utils/errorReport';
  import { addToast } from '../../stores/toast.svelte';

  interface Props {
    captured: CapturedError;
    expanded: boolean;
    onToggle: () => void;
  }

  let { captured, expanded, onToggle }: Props = $props();
</script>

{#if errorHasDetails(captured.report)}
  <IconButton
    label={expanded ? 'Hide details' : 'Show details'}
    size="xs"
    variant="tint"
    ariaExpanded={expanded}
    testId="error-details-toggle"
    onClick={onToggle}
  >
    {#snippet children()}
      <Icon icon={ChevronDown} size={13} strokeWidth={2.25} class="transition-transform duration-150 {expanded ? 'rotate-180' : ''}" />
    {/snippet}
  </IconButton>
{/if}
<CopyButton
  text={() => capturedErrorText(captured)}
  label="Copy error for an agent"
  copiedLabel="Copied"
  size="xs"
  iconSize={12}
  variant="tint"
  onError={() => addToast('error', 'Failed to copy')}
/>
