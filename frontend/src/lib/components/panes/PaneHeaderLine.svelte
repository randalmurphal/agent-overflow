<script lang="ts">
  // The pane header's status line: an accent-family hairline laid over the
  // header's bottom border, the same line the composer's activity rail draws
  // across its top while a turn runs. Color and presence come from
  // paneHeaderLine.ts. The host header must be `relative`; the line sits at
  // -1px so it covers the separator instead of stacking above it.
  import type { Thread } from '../../types/models';
  import { paneHeaderLineFor } from './paneHeaderLine';

  interface Props {
    paneId: string;
    /** The pane's thread, for attention states. Omit on panes without one. */
    thread?: Thread | null;
  }

  let { paneId, thread = null }: Props = $props();

  let line = $derived(paneHeaderLineFor(paneId, thread));
</script>

{#if line}
  <span
    class="accent-hairline pointer-events-none absolute inset-x-0 -bottom-px z-10 block h-px"
    style:--hairline-color={`var(--${line.color})`}
    aria-hidden="true"
    data-testid="pane-header-line"
    data-line={line.color}
  ></span>
  <span class="sr-only" data-testid="pane-header-line-label">{line.label}</span>
{/if}
