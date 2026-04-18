<!--
  Test harness for <CompactHeaderMenu>. Snippets can't be constructed
  from a .ts file, so the test renders this wrapper and passes a plain
  text body through the exposed props. We surface the `onClose`
  callback on a button so the test can invoke it directly to cover the
  callback-closes-menu contract.
-->
<script lang="ts">
  import CompactHeaderMenu from './CompactHeaderMenu.svelte';

  let {
    label = 'More',
    bodyText = 'menu-body',
  }: {
    label?: string;
    bodyText?: string;
  } = $props();
</script>

<CompactHeaderMenu {label}>
  {#snippet children({ onClose })}
    <span data-testid="menu-body">{bodyText}</span>
    <button
      type="button"
      data-testid="menu-close-from-child"
      onclick={onClose}
    >
      close-from-child
    </button>
    <button
      type="button"
      data-testid="menu-focus-last"
    >
      last
    </button>
  {/snippet}
</CompactHeaderMenu>
