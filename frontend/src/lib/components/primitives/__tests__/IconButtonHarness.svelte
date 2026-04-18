<!--
  Test harness for <IconButton>. Snippets (the icon child) can't be
  constructed from a .ts test file, so the harness fills the children
  slot with a deterministic <span data-testid="icon"> that the test can
  assert on.
-->
<script lang="ts">
  import IconButton from '../IconButton.svelte';

  let {
    label = 'Do thing',
    disabled = false,
    size = 'md' as 'sm' | 'md',
    variant = 'ghost' as 'ghost' | 'subtle',
    onClick = undefined as ((e: MouseEvent) => void) | undefined,
  }: {
    label?: string;
    disabled?: boolean;
    size?: 'sm' | 'md';
    variant?: 'ghost' | 'subtle';
    onClick?: (e: MouseEvent) => void;
  } = $props();
</script>

<IconButton {label} {disabled} {size} {variant} {onClick}>
  {#snippet children()}
    <span data-testid="icon">o</span>
  {/snippet}
</IconButton>
