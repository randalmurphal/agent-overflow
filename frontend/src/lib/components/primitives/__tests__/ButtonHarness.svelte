<!--
  Test harness for <Button>. Snippets can't be constructed from .ts, so
  the harness fills `children`, `leading`, and `trailing` slots with
  deterministic markers the tests assert on. All snippet blocks are
  emitted explicitly (no implicit children content) so Svelte 5 doesn't
  flag a snippet_conflict.
-->
<script lang="ts">
  import Button from '../Button.svelte';

  type Variant =
    | 'primary'
    | 'secondary'
    | 'ghost'
    | 'tinted'
    | 'danger'
    | 'danger-outline'
    | 'danger-ghost';
  type Size = 'xs' | 'sm' | 'md';
  type ButtonType = 'button' | 'submit' | 'reset';

  let {
    variant = 'secondary' as Variant,
    size = 'sm' as Size,
    type = 'button' as ButtonType,
    disabled = false,
    loading = false,
    pressed = undefined as boolean | undefined,
    title = undefined as string | undefined,
    ariaLabel = undefined as string | undefined,
    onclick = undefined as ((e: MouseEvent) => void) | undefined,
    label = 'Click me',
    withLeading = false,
    withTrailing = false,
  }: {
    variant?: Variant;
    size?: Size;
    type?: ButtonType;
    disabled?: boolean;
    loading?: boolean;
    pressed?: boolean;
    title?: string;
    ariaLabel?: string;
    onclick?: (e: MouseEvent) => void;
    label?: string;
    withLeading?: boolean;
    withTrailing?: boolean;
  } = $props();
</script>

{#if withLeading && withTrailing}
  <Button {variant} {size} {type} {disabled} {loading} {pressed} {title} {ariaLabel} {onclick}>
    {#snippet children()}{label}{/snippet}
    {#snippet leading()}<span data-testid="leading">L</span>{/snippet}
    {#snippet trailing()}<span data-testid="trailing">T</span>{/snippet}
  </Button>
{:else if withLeading}
  <Button {variant} {size} {type} {disabled} {loading} {pressed} {title} {ariaLabel} {onclick}>
    {#snippet children()}{label}{/snippet}
    {#snippet leading()}<span data-testid="leading">L</span>{/snippet}
  </Button>
{:else if withTrailing}
  <Button {variant} {size} {type} {disabled} {loading} {pressed} {title} {ariaLabel} {onclick}>
    {#snippet children()}{label}{/snippet}
    {#snippet trailing()}<span data-testid="trailing">T</span>{/snippet}
  </Button>
{:else}
  <Button {variant} {size} {type} {disabled} {loading} {pressed} {title} {ariaLabel} {onclick}>
    {#snippet children()}{label}{/snippet}
  </Button>
{/if}
