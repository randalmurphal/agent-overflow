<!--
  Harness for <Modal>: passes predictable body + footer snippets so tests
  can tab between focusable controls and probe backdrop-click behaviour.
-->
<script lang="ts">
  import Modal from '../Modal.svelte';

  let {
    open = true,
    title = 'Test Modal',
    onClose = () => {},
    width = 'md' as 'sm' | 'md' | 'lg' | 'xl',
    padding = 'comfortable' as 'tight' | 'comfortable' | 'loose',
    align = 'center' as 'center' | 'top',
    withFooter = true,
    withHeaderActions = false,
  }: {
    open?: boolean;
    title?: string;
    onClose?: () => void;
    width?: 'sm' | 'md' | 'lg' | 'xl';
    padding?: 'tight' | 'comfortable' | 'loose';
    align?: 'center' | 'top';
    withFooter?: boolean;
    withHeaderActions?: boolean;
  } = $props();
</script>

{#if withFooter && withHeaderActions}
  <Modal {open} {title} {onClose} {width} {padding} {align}>
    {#snippet children()}
      <input data-testid="modal-first" type="text" />
      <button data-testid="modal-middle" type="button">middle</button>
    {/snippet}
    {#snippet footer()}
      <button data-testid="modal-last" type="button">last</button>
    {/snippet}
    {#snippet headerActions()}
      <button data-testid="modal-header-action" type="button">x</button>
    {/snippet}
  </Modal>
{:else if withFooter}
  <Modal {open} {title} {onClose} {width} {padding} {align}>
    {#snippet children()}
      <input data-testid="modal-first" type="text" />
      <button data-testid="modal-middle" type="button">middle</button>
    {/snippet}
    {#snippet footer()}
      <button data-testid="modal-last" type="button">last</button>
    {/snippet}
  </Modal>
{:else if withHeaderActions}
  <Modal {open} {title} {onClose} {width} {padding} {align}>
    {#snippet children()}
      <input data-testid="modal-first" type="text" />
      <button data-testid="modal-middle" type="button">middle</button>
    {/snippet}
    {#snippet headerActions()}
      <button data-testid="modal-header-action" type="button">x</button>
    {/snippet}
  </Modal>
{:else}
  <Modal {open} {title} {onClose} {width} {padding} {align}>
    {#snippet children()}
      <input data-testid="modal-first" type="text" />
      <button data-testid="modal-middle" type="button">middle</button>
    {/snippet}
  </Modal>
{/if}
