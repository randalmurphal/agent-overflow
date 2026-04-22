<!--
  Harness for the Modal "palette shape": no title, custom `header`
  snippet replaces the default chrome, and `padding='none'` removes
  body padding. Separated from ModalHarness to keep each harness
  small and avoid another layer of conditional-snippet branching.
-->
<script lang="ts">
  import Modal from '../Modal.svelte';

  let {
    open = true,
    onClose = () => {},
    ariaLabel = 'Command palette',
    withHeader = false,
    padding = 'none' as 'none' | 'tight' | 'comfortable' | 'loose',
  }: {
    open?: boolean;
    onClose?: () => void;
    ariaLabel?: string;
    withHeader?: boolean;
    padding?: 'none' | 'tight' | 'comfortable' | 'loose';
  } = $props();
</script>

{#if withHeader}
  <Modal {open} {onClose} {ariaLabel} {padding}>
    {#snippet header()}
      <div class="palette-header" data-testid="modal-custom-header">
        <input type="text" data-testid="modal-first" placeholder="Type…" />
      </div>
    {/snippet}
    {#snippet children()}
      <div data-testid="modal-body">body</div>
    {/snippet}
  </Modal>
{:else}
  <Modal {open} {onClose} {ariaLabel} {padding}>
    {#snippet children()}
      <div data-testid="modal-body">body</div>
    {/snippet}
  </Modal>
{/if}
