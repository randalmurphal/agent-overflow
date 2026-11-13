<script lang="ts" module>
  import type { ReviewFile } from '../../utils/patchStore';

  /** Where a suggestion's replaced lines are read from: new-side lines
   * `from`..`to` of a file of the loaded diff. */
  export interface SuggestionSource {
    readonly file: ReviewFile;
    readonly from: number;
    readonly to: number;
  }
</script>

<script lang="ts">
  import type { Snippet } from 'svelte';
  import ChatMarkdown from '../chat/ChatMarkdown.svelte';
  import { EMPTY_PATH_REFS } from '../../utils/pathLinkify';
  import { layoutBody, type BodyRange } from '../../utils/suggestionPieces';

  // A comment body as the review pane renders it, sent or previewed: the
  // markdown in ChatMarkdown's sanitized embedded mode, cut at the
  // suggestion fences the backend placed (`ranges`), each block rendered
  // in place by `block` as the diff it proposes. A body the blocks cannot
  // all be placed in renders whole, its fences as code, and the blocks
  // follow it (utils/suggestionPieces.ts).

  interface Props {
    /** The raw body, as the forge holds it: the ranges count its lines. */
    body: string;
    ranges?: readonly (BodyRange | null)[];
    /** Renders block `at` (an index into `ranges`). */
    block?: Snippet<[number]>;
    class?: string;
  }

  let { body, ranges = [], block, class: className = '' }: Props = $props();

  const layout = $derived(layoutBody(body, ranges));
</script>

{#if layout.pieces.length === 1 && layout.pieces[0].kind === 'markdown' && layout.after.length === 0}
  <ChatMarkdown source={layout.pieces[0].text} pathRefs={EMPTY_PATH_REFS} embeddedHtml class="review-prose {className}" />
{:else}
  <div class={className}>
    {#each layout.pieces as piece (piece.key)}
      {#if piece.kind === 'markdown'}
        <ChatMarkdown source={piece.text} pathRefs={EMPTY_PATH_REFS} embeddedHtml class="review-prose" />
      {:else}
        {@render block?.(piece.at)}
      {/if}
    {/each}
    {#each layout.after as at (at)}
      {@render block?.(at)}
    {/each}
  </div>
{/if}
