<script lang="ts">
  // Long text in an AO tool row's body, folded to its first lines with a
  // "show more" toggle. Unfolded it still stops at `max` characters; the
  // body's copy button carries the whole reply.
  import type { PaneSession, RowUiRegistry, ScrollHost } from '../../stores/threadPaneRoles';
  import AnsiText from './AnsiText.svelte';
  import { preservePaneScrollAnchor } from './preserveScrollAnchor';

  let {
    pane,
    text,
    lines,
    max,
    mono = false,
    testId,
  }: {
    pane?: PaneSession & RowUiRegistry & ScrollHost;
    text: string;
    lines: number;
    max: number;
    mono?: boolean;
    testId: string;
  } = $props();

  // A fold at the line limit, or at a character budget for one very long line.
  const FOLDED_CHARS = 600;
  let open = $state(false);
  let folded = $derived.by(() => {
    let cut = -1;
    let from = 0;
    for (let line = 0; line < lines; line++) {
      cut = text.indexOf('\n', from);
      if (cut < 0) break;
      from = cut + 1;
    }
    const end = cut < 0 ? text.length : cut;
    return text.slice(0, Math.min(end, FOLDED_CHARS));
  });
  let foldable = $derived(folded.length < text.length);
  let shown = $derived(
    !foldable ? text : open ? (text.length > max ? `${text.slice(0, max - 1)}…` : text) : `${folded.trimEnd()}…`,
  );
</script>

<div class="min-w-0" data-testid={testId} data-folded={foldable && !open ? 'true' : 'false'}>
  <AnsiText source={shown} class="whitespace-pre-wrap break-words {mono ? 'font-mono' : ''}" />{#if foldable}<button
      type="button"
      class="mt-0.5 block text-accent hover:underline focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/50 rounded compact:select-none"
      aria-expanded={open}
      data-testid="{testId}-toggle"
      onclick={(event) => {
        event.stopPropagation();
        preservePaneScrollAnchor(pane, event, () => {
          open = !open;
        });
      }}
    >{open ? 'Show less' : 'Show more'}</button>{/if}
</div>
