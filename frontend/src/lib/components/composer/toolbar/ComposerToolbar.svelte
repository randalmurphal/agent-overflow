<script lang="ts">
  // Bottom row of the composer box. Pure layout — every child owns its
  // own binding calls, so this file's only job is to arrange them
  // left-to-right and surface the send button on the right edge.
  //
  // The top border pins the toolbar to the textarea above it so the
  // whole composer reads as one control. The send button uses `ml-auto`
  // for a right-hanging position that survives adding new child
  // controls (e.g. workflows when they land).

  import type { ThreadPane } from '../../../stores/thread.svelte';
  import ModelProviderMenu from './ModelProviderMenu.svelte';
  import EffortMenu from './EffortMenu.svelte';
  import ModeCycleButton from './ModeCycleButton.svelte';
  import AccessToggle from './AccessToggle.svelte';
  import SendButton from './SendButton.svelte';

  interface Props {
    pane: ThreadPane;
    canSend: boolean;
    isTurnActive: boolean;
    onSend: () => void;
    onInterrupt: () => void;
  }

  let { pane, canSend, isTurnActive, onSend, onInterrupt }: Props = $props();
</script>

<div
  class="flex items-center gap-1 px-2 py-1.5 border-t border-border bg-surface-1"
  data-testid="composer-toolbar"
>
  <ModelProviderMenu {pane} />
  <EffortMenu {pane} />
  <ModeCycleButton {pane} />
  <AccessToggle {pane} />
  <SendButton {canSend} {isTurnActive} {onSend} {onInterrupt} />
</div>
