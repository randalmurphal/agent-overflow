<script lang="ts">
  import ReviewCommentMarkdown, { type SuggestionSource } from './ReviewCommentMarkdown.svelte';

  // An editor's text as it will read, in the text box's place: the same
  // rendering a sent comment gets (ReviewCommentMarkdown: ChatMarkdown's
  // sanitized embedded mode, each suggestion block as the diff it will
  // become, its replaced lines read from `source`, the line or range the
  // comment is on). It stands at least as tall as the box it replaces,
  // so the editor does not jump.

  interface Props {
    body: string;
    /** The text box's height when preview turned on, in px. */
    height: number;
    source?: SuggestionSource | null;
    class?: string;
    testid?: string;
  }

  let { body, height, source = null, class: className = '', testid = 'review-editor-preview' }: Props = $props();

  // A widened GitLab block replaces lines past the anchor.
  function sourceFor(block: { above: number; below: number }): SuggestionSource | null {
    if (!source) return null;
    return { file: source.file, from: Math.max(1, source.from - block.above), to: source.to + block.below };
  }
</script>

<div
  class="overflow-auto rounded-[var(--radius-field)] border border-border-subtle bg-surface-0 px-2.5 py-2 text-xs text-fg {className}"
  style:min-height="{height}px"
  data-testid={testid}
>
  {#if body.trim() === ''}
    <span class="text-fg-subtle">Nothing to preview.</span>
  {:else}
    <ReviewCommentMarkdown {body} {sourceFor} />
  {/if}
</div>
