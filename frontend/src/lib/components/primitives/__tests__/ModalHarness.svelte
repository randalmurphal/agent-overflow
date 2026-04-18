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
    width = 'md' as 'sm' | 'md' | 'lg',
    withFooter = true,
  }: {
    open?: boolean;
    title?: string;
    onClose?: () => void;
    width?: 'sm' | 'md' | 'lg';
    withFooter?: boolean;
  } = $props();
</script>

{#if withFooter}
  <Modal {open} {title} {onClose} {width}>
    {#snippet children()}
      <input data-testid="modal-first" type="text" />
      <button data-testid="modal-middle" type="button">middle</button>
    {/snippet}
    {#snippet footer()}
      <button data-testid="modal-last" type="button">last</button>
    {/snippet}
  </Modal>
{:else}
  <Modal {open} {title} {onClose} {width}>
    {#snippet children()}
      <input data-testid="modal-first" type="text" />
      <button data-testid="modal-middle" type="button">middle</button>
    {/snippet}
  </Modal>
{/if}
