<!--
  Test harness for <PaneCloseButton>. It wraps the button in an ancestor
  that mirrors the real pane chrome: the pane section (PaneHost) listens
  for pointerdown (focus + transition reveal) and focusin (logical focus).
  The close button must stop both so closing an unfocused pane neither
  scrolls it into view nor moves logical focus onto the dying pane. The
  spies stand in for those ancestor handlers; Svelte's delegated dispatch
  honours stopPropagation, so a stopped event never reaches them.
-->
<script lang="ts">
  import PaneCloseButton from './PaneCloseButton.svelte';

  let {
    paneId,
    onAncestorFocusIn,
    onAncestorPointerDown,
  }: {
    paneId: string;
    onAncestorFocusIn?: (event: FocusEvent) => void;
    onAncestorPointerDown?: (event: PointerEvent) => void;
  } = $props();
</script>

<!-- svelte-ignore a11y_no_static_element_interactions -->
<div
  data-testid="pane-close-ancestor"
  onfocusin={onAncestorFocusIn}
  onpointerdown={onAncestorPointerDown}
>
  <PaneCloseButton {paneId} />
</div>
