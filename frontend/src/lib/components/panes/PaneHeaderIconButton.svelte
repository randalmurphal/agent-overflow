<script lang="ts">
  // Shared chrome for the pane header's h-5 icon affordances (close, back,
  // keep, open as pane): one class string, one pointerdown stop, one click
  // stop.
  // The pointerdown stop keeps a press from starting the header's pane-drag
  // gesture and from being treated as a pane-focus transition by PaneHost's
  // pointer-focus handler. Behavior only one consumer needs (PaneCloseButton's
  // focusin stop) rides in as an optional prop rather than that consumer
  // copying the whole button.
  import type { Snippet } from 'svelte';

  interface Props {
    /** Accessible name; also the tooltip unless `title` overrides it. */
    label: string;
    title?: string;
    disabled?: boolean;
    testId: string;
    onclick: () => void;
    onfocusin?: (event: FocusEvent) => void;
    children: Snippet;
  }

  let {
    label,
    title = undefined,
    disabled = false,
    testId,
    onclick,
    onfocusin = undefined,
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
  class="flex h-5 w-5 compact:h-9 compact:w-9 shrink-0 items-center justify-center rounded-[var(--radius-field)] text-fg-hint opacity-70 transition-colors hover:bg-surface-2/70 hover:text-fg focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/40 disabled:pointer-events-none disabled:opacity-40"
>
  {@render children()}
</button>
