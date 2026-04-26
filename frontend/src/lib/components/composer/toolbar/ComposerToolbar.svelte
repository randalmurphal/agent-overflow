<script lang="ts">
  // Bottom row of the composer card. Pure layout — every child owns its
  // own binding calls, so this file's only job is to arrange them
  // left-to-right and surface the send button on the right edge.
  //
  // The toolbar now sits inside the composer card (no border-t, no
  // bg-surface-1 of its own) so the whole composer reads as one
  // continuous surface. Thin vertical separators mark logical groups:
  // model/effort (agent config) | mode/access (execution config) | send.

  import type { ThreadPane } from '../../../stores/thread.svelte';
  import Separator from '../../primitives/Separator.svelte';
  import ModelProviderMenu from './ModelProviderMenu.svelte';
  import EffortMenu from './EffortMenu.svelte';
  import ModeCycleButton from './ModeCycleButton.svelte';
  import AccessToggle from './AccessToggle.svelte';
  import SendButton from './SendButton.svelte';

  interface Props {
    pane: ThreadPane;
    canSend: boolean;
    isTurnActive: boolean;
    sendLabel?: string;
    onSend: () => void;
    onInterrupt: () => void;
  }

  let { pane, canSend, isTurnActive, sendLabel, onSend, onInterrupt }: Props = $props();
</script>

<div
  class="flex items-center gap-0.5 px-2.5 pb-2 pt-1"
  data-testid="composer-toolbar"
>
  <ModelProviderMenu {pane} />
  <EffortMenu {pane} />
  <Separator orientation="vertical" opacity={0.4} class="h-4 mx-1" />
  <ModeCycleButton {pane} />
  <AccessToggle {pane} />
  <div class="ml-auto flex items-center">
    <SendButton {canSend} {isTurnActive} label={sendLabel} {onSend} {onInterrupt} />
  </div>
</div>
