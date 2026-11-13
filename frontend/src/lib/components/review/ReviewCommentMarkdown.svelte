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
  import type { CodeClaim } from '../chat/markdown/codeClaim';
  import { EMPTY_PATH_REFS } from '../../utils/pathLinkify';
  import { markSuggestionFences, suggestionMarkIndex, type PlacedBlock } from '../../utils/suggestionMarks';

  // A comment body as the review pane renders it, sent or previewed: one
  // Markdown document (ChatMarkdown's sanitized embedded mode), each
  // suggestion fence the backend placed rendered in place by `block` as
  // the diff it proposes, so `<details>`, footnotes and lists around it
  // keep their structure. A block that is not placed, or whose fence no
  // longer holds its lines, leaves the fence as code and renders after
  // the body (utils/suggestionMarks.ts).

  interface Props {
    /** The raw body, as the forge holds it: the blocks count its lines. */
    body: string;
    blocks?: readonly (PlacedBlock | null)[];
    /** Renders block `at` (an index into `blocks`). */
    block?: Snippet<[number]>;
    class?: string;
  }

  let { body, blocks = [], block, class: className = '' }: Props = $props();

  const marked = $derived(markSuggestionFences(body, blocks));
  const claim = $derived<CodeClaim | null>(blocks.length === 0 || !block ? null : {
    claims: (token) => suggestionMarkIndex(token.lang ?? '') !== null,
    render: claimed,
  });
</script>

{#snippet claimed(token: { lang?: string })}
  {@const at = suggestionMarkIndex(token.lang ?? '')}
  {#if at !== null}{@render block?.(at)}{/if}
{/snippet}

<ChatMarkdown source={marked.source} pathRefs={EMPTY_PATH_REFS} embeddedHtml codeClaim={claim} class="review-prose {className}" />
{#each marked.after as at (at)}
  {@render block?.(at)}
{/each}
