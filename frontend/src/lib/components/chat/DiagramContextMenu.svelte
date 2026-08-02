<script lang="ts">
  /**
   * Right-click menu for a rendered mermaid diagram. Positioning,
   * click-outside and Escape live in the shared `ContextMenu` shell;
   * this component owns only the action rows.
   */

  import ContextMenu from '../primitives/ContextMenu.svelte';
  import MenuItem from '../primitives/MenuItem.svelte';
  import MenuDivider from '../primitives/MenuDivider.svelte';

  export type DiagramAction = 'copy-png' | 'copy-svg' | 'copy-source' | 'expand' | 'close';

  interface Props {
    x: number;
    y: number;
    context: 'inline' | 'modal';
    onAction: (action: DiagramAction) => void;
    onDismiss: () => void;
  }

  let { x, y, context, onAction, onDismiss }: Props = $props();
</script>

<ContextMenu {x} {y} ariaLabel="Diagram Actions" {onDismiss}>
  <MenuItem label="Copy as PNG" onSelect={() => onAction('copy-png')} />
  <MenuItem label="Copy as SVG" onSelect={() => onAction('copy-svg')} />
  <MenuItem label="Copy Source" onSelect={() => onAction('copy-source')} />
  <MenuDivider />
  {#if context === 'inline'}
    <MenuItem label="Expand" onSelect={() => onAction('expand')} />
  {:else}
    <MenuItem label="Close" onSelect={() => onAction('close')} />
  {/if}
</ContextMenu>
