<script lang="ts">
  import ReviewCommentMarkdown, { type SuggestionSource } from './ReviewCommentMarkdown.svelte';
  import ReviewSuggestionBlock from './ReviewSuggestionBlock.svelte';
  import type { SuggestionParse } from '../../stores/reviewEditorPreview.svelte';
  import { userFacingError } from '../../utils/userFacingError';
  import type { Block } from '../../../../bindings/agent-overflow/internal/mdsuggest/models';

  // An editor's text as it will read, in the text box's place: the same
  // rendering a sent comment gets (ReviewCommentMarkdown: ChatMarkdown's
  // sanitized embedded mode), each suggestion block as the diff it will
  // become, its replaced lines read from `source`, the line or range the
  // comment is on. Where the blocks are is the backend's answer (`parse`),
  // asked when the preview opens and again for a changed text; an answer
  // for an older text, or one arriving after the preview closed, is
  // dropped. Until the answer for this text is in, or when the backend
  // refuses (a body over its bound), the markdown shows with its fences as
  // code. It stands exactly as tall as the box it replaces and scrolls
  // inside, as the box does, so the editor does not jump.

  interface Props {
    body: string;
    /** The text box's height when preview turned on, in px. */
    height: number;
    source?: SuggestionSource | null;
    /** Reads the body's suggestion blocks; absent, none are shown. */
    parse?: SuggestionParse | null;
    class?: string;
    testid?: string;
  }

  let { body, height, source = null, parse = null, class: className = '', testid = 'review-editor-preview' }: Props = $props();

  let answer = $state.raw<{ body: string; blocks: readonly Block[]; error: string | null } | null>(null);

  $effect(() => {
    const text = body;
    const read = parse;
    if (!read || text.trim() === '') return;
    // An older read's answer is dropped even when it lands after this
    // one's; `current` also never lays a text out by another's answer.
    let live = true;
    read(text).then(
      (blocks) => { if (live) answer = { body: text, blocks, error: null }; },
      (err: unknown) => { if (live) answer = { body: text, blocks: [], error: userFacingError(err) }; },
    );
    return () => { live = false; };
  });

  const current = $derived(answer !== null && answer.body === body ? answer : null);
  const blocks = $derived(current?.blocks ?? []);
  const ranges = $derived(blocks.map((block) => ({ startLine: block.startLine, endLine: block.endLine })));

  // A widened GitLab block replaces lines past the anchor.
  function sourceFor(block: Block): SuggestionSource | null {
    if (!source) return null;
    return { file: source.file, from: Math.max(1, source.from - (block.above ?? 0)), to: source.to + (block.below ?? 0) };
  }
</script>

{#snippet suggestionBlock(at: number)}
  {@const block = blocks[at]}
  {@const from = sourceFor(block)}
  <ReviewSuggestionBlock added={block.lines} file={from?.file ?? null} from={from?.from ?? 0} to={from?.to ?? 0} />
{/snippet}

<div
  class="overflow-auto rounded-[var(--radius-field)] border border-border-subtle bg-surface-0 px-2.5 py-2 text-xs text-fg {className}"
  style:height={height > 0 ? `${height}px` : null}
  data-testid={testid}
>
  {#if body.trim() === ''}
    <span class="text-fg-subtle">Nothing to preview.</span>
  {:else}
    {#if current?.error}
      <p class="mb-1.5 text-[0.6875rem] text-fg-subtle" data-testid="review-editor-preview-note">Suggestions are shown as code: {current.error}</p>
    {/if}
    <ReviewCommentMarkdown {body} {ranges} block={suggestionBlock} />
  {/if}
</div>
