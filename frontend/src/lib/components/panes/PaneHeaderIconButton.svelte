<script lang="ts">
  // Shared chrome for the pane header's h-5 icon affordances (close, title
  // regenerate): one class string, one pointerdown stop, one click stop.
  // The pointerdown stop keeps a press from starting the header's pane-drag
  // gesture and from being treated as a pane-focus transition by PaneHost's
  // pointer-focus handler. Behavior only one consumer needs (PaneCloseButton's
  // focusin stop, the regenerate button's pending marker) rides in as optional
  // props rather than each consumer copying the whole button.
  import type { Snippet } from 'svelte';

  interface Props {
    /** Accessible name; also the tooltip unless `title` overrides it. */
    label: string;
    title?: string;
    disabled?: boolean;
    testId: string;
    onclick: () => void;
    onfocusin?: (event: FocusEvent) => void;
    /** Rendered as `data-pending` when set; omitted when undefined. */
    pending?: boolean;
    children: Snippet;
  }

  let {
    label,
    title = undefined,
    disabled = false,
    testId,
    onclick,
    onfocusin = undefined,
    pending = undefined,
    children,
  }: Props = $props();
</script>

<button
  type="button"
  aria-label={label}
  title={title ?? label}
  {disabled}
  onpointerdown={(event) => event.stopPropagation()}
  {onfocusin}
  onclick={(event) => {
    event.stopPropagation();
    onclick();
  }}
  data-testid={testId}
  data-pending={pending}
  class="flex h-5 w-5 shrink-0 items-center justify-center rounded-[var(--radius-field)] text-fg-hint opacity-70 transition-colors hover:bg-surface-2/70 hover:text-fg focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/40 disabled:pointer-events-none disabled:opacity-40"
>
  {@render children()}
</button>
