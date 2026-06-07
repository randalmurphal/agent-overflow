<script lang="ts">
  // The pane-header close affordance: an X that destroys the pane. The
  // underlying session keeps running — closing removes the pane, it never
  // calls CloseTerminal / StopSession, so reopening the thread reattaches.
  // Shared by ChatHeader and the terminal pane header so both get identical
  // markup, sizing, and the propagation stops that keep pane-level handlers
  // from reacting to a click on the X:
  //   - pointerdown: stops a header drag from starting on the button.
  //   - focusin: the button takes focus on click, and both the pane section
  //     (PaneHost) and the chat column (ChatView) focus the pane on focusin —
  //     focusing reveals (scroll-into-view) the pane. Without this stop,
  //     closing a partially-scrolled pane first smooth-scrolls it on-screen
  //     and then closes it, a jarring shift. Stopping focusin lets the click
  //     destroy the pane in place.
  import X from 'lucide-svelte/icons/x';
  import Icon from '../primitives/Icon.svelte';
  import { destroyPane } from '../../stores/panes.svelte';

  let {
    paneId,
    testId = 'pane-close',
  }: {
    paneId: string;
    testId?: string;
  } = $props();
</script>

<button
  type="button"
  aria-label="Close Pane"
  title="Close Pane"
  onpointerdown={(event) => event.stopPropagation()}
  onfocusin={(event) => event.stopPropagation()}
  onclick={(event) => {
    event.stopPropagation();
    destroyPane(paneId);
  }}
  data-testid={testId}
  class="flex h-5 w-5 shrink-0 items-center justify-center rounded-[var(--radius-field)] text-fg-hint opacity-70 transition-colors hover:bg-surface-2/70 hover:text-fg focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/40"
>
  <Icon icon={X} size={12} strokeWidth={2} />
</button>
