<!--
  Test harness for <PaneCloseButton>. It wraps the button in an ancestor
  that mirrors the real pane chrome: both the pane section (PaneHost) and the
  chat column (ChatView) listen for focusin / pointerdown to focus + reveal
  the pane. The close button must stop both so closing a partially-scrolled
  pane doesn't first scroll it into view. The spies stand in for those
  ancestor handlers; Svelte's delegated dispatch honours stopPropagation, so
  a stopped event never reaches them.
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
