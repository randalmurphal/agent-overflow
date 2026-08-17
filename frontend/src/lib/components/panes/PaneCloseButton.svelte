<script lang="ts">
  // The pane-header close affordance: an X that destroys the pane. The
  // underlying session keeps running — closing removes the pane, it never
  // calls CloseTerminal / StopSession, so reopening the thread reattaches.
  // Shared by ChatHeader and the terminal pane header. Chrome and the
  // pointerdown stop live in PaneHeaderIconButton; the focusin stop is this
  // button's own: Chromium-engine webviews focus buttons on mousedown, so
  // without it the X's focusin would move LOGICAL focus onto the pane being
  // destroyed, and destroyPane's dangling-focus fixup would then focus +
  // reveal its neighbor — stealing focus from the pane the user was actually
  // working in. (focusin no longer scrolls anywhere; the stop is purely about
  // keeping logical focus off the dying pane.)
  import X from '@lucide/svelte/icons/x';
  import Icon from '../primitives/Icon.svelte';
  import PaneHeaderIconButton from './PaneHeaderIconButton.svelte';
  import { destroyPane } from '../../stores/panes.svelte';

  let {
    paneId,
    testId = 'pane-close',
  }: {
    paneId: string;
    testId?: string;
  } = $props();
</script>

<PaneHeaderIconButton
  label="Close Pane"
  {testId}
  onclick={() => destroyPane(paneId)}
  onfocusin={(event) => event.stopPropagation()}
>
  <Icon icon={X} size={12} strokeWidth={2} />
</PaneHeaderIconButton>
