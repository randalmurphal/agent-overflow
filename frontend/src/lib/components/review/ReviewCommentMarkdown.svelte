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
  import ReviewSuggestionBlock from './ReviewSuggestionBlock.svelte';
  import { EMPTY_PATH_REFS } from '../../utils/pathLinkify';
  import { suggestionSegments, type SuggestionSegment } from '../../utils/suggestionFences';
  import type { CommentSuggestion } from '../../types/models';

  // A comment body as the review pane renders it, sent or previewed: the
  // markdown in ChatMarkdown's sanitized embedded mode, and each
  // suggestion block in place as the diff it proposes (the fence is not
  // shown as well). A sent comment passes the forge's suggestions, matched
  // to the blocks by body order; a preview passes none and the blocks'
  // own lines are the additions. `sourceFor` says where a block's
  // replaced lines are read when the forge did not report them.

  interface Props {
    body: string;
    suggestions?: readonly CommentSuggestion[] | null;
    sourceFor?: (block: Extract<SuggestionSegment, { kind: 'suggestion' }>, suggestion: CommentSuggestion | null) => SuggestionSource | null;
    /** Under a block: its Apply, Applied or Outdated, and a refusal. */
    footer?: Snippet<[CommentSuggestion]>;
    /** Whether a block's footer has anything to show. */
    showFooter?: (suggestion: CommentSuggestion) => boolean;
    class?: string;
  }

  let { body, suggestions = null, sourceFor, footer, showFooter, class: className = '' }: Props = $props();

  const segments = $derived(suggestionSegments(body));
  // Segments have no ids of their own: a block is its index in the body
  // (CommentSuggestion.index), the markdown before it is named by it.
  function keyOf(segment: SuggestionSegment, at: number): string {
    if (segment.kind === 'suggestion') return `suggestion:${segment.index}`;
    const next = segments.slice(at + 1).find((each) => each.kind === 'suggestion');
    return next?.kind === 'suggestion' ? `before:${next.index}` : 'tail';
  }
</script>

{#if segments.length === 1 && segments[0].kind === 'markdown'}
  <ChatMarkdown source={body} pathRefs={EMPTY_PATH_REFS} embeddedHtml class="review-prose {className}" />
{:else}
  <div class={className}>
    {#each segments as segment, at (keyOf(segment, at))}
      {#if segment.kind === 'markdown'}
        <ChatMarkdown source={segment.text} pathRefs={EMPTY_PATH_REFS} embeddedHtml class="review-prose" />
      {:else}
        {@const suggestion = suggestions?.find((each) => each.index === segment.index) ?? null}
        {@const source = suggestion?.fromKnown ? null : (sourceFor?.(segment, suggestion) ?? null)}
        <ReviewSuggestionBlock
          added={suggestion?.to ?? segment.lines}
          removedKnown={suggestion?.fromKnown ? (suggestion.from ?? []) : null}
          file={source?.file ?? null}
          from={source?.from ?? 0}
          to={source?.to ?? 0}
          footer={suggestion && footer && (showFooter?.(suggestion) ?? true) ? suggestionFooter : undefined}
        />
        {#snippet suggestionFooter()}
          {#if suggestion}{@render footer?.(suggestion)}{/if}
        {/snippet}
      {/if}
    {/each}
  </div>
{/if}
