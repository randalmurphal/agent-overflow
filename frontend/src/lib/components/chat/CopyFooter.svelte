<script lang="ts">
  // Footer bar that lives at the bottom of an expanded chat block
  // (thinking, command output, generic tool call, tool result patch).
  // Centralizes the wrapper styling and the failure-toast wiring so
  // each block's call site stays a single line.
  import CopyButton from '../primitives/CopyButton.svelte';
  import { addToast } from '../../stores/toast.svelte';

  interface Props {
    text: string | (() => string | Promise<string>);
    label: string;
  }

  let { text, label }: Props = $props();
</script>

<div class="flex justify-end border-t border-border-subtle/40 px-1 py-0.5">
  <CopyButton {text} {label} onError={() => addToast('error', 'Failed to copy')} />
</div>
