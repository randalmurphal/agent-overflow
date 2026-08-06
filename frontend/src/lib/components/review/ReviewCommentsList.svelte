<script lang="ts">
  import { SvelteSet } from 'svelte/reactivity';
  import FileText from '@lucide/svelte/icons/file-text';
  import MessageSquare from '@lucide/svelte/icons/message-square';
  import ChatMarkdown from '../chat/ChatMarkdown.svelte';
  import Icon from '../primitives/Icon.svelte';
  import type { ReviewVerdict } from '../../types/models';
  import type { CommentFileGroup, CommentItemState, CommentListItem } from '../../utils/reviewComments';
  import { commentSnippet } from '../../utils/reviewComments';
  import { relativeTime } from '../../utils/format';

  // The rail's Comments tab: every PR thread (file-anchored and PR-level
  // conversation) and local draft in the current scope, grouped by file,
  // actionable items first. The snippet is the row's primary text —
  // author/line/state are demoted to a small meta line (full author on
  // hover). Clicking an in-diff item jumps the diff body to its row;
  // items with no diff row (conversation threads, files outside the
  // diff) expand inline so their full text is still readable here.
  // PR-scope review summaries (approve / request-changes verdict bodies)
  // render as a group at the top — they have no diff anchor.

  interface Props {
    groups: readonly CommentFileGroup[];
    reviews?: readonly ReviewVerdict[];
    onSelect: (item: CommentListItem) => void;
  }

  let { groups, reviews = [], onSelect }: Props = $props();

  const empty = $derived(groups.length === 0 && reviews.length === 0);

  // Expansion is view-local by design: the list is not virtualized and
  // holds no user input, so losing it on a tab switch is fine.
  const expandedKeys = new SvelteSet<string>();

  function activate(item: CommentListItem): void {
    if (item.inDiff) {
      onSelect(item);
      return;
    }
    if (expandedKeys.has(item.rowKey)) expandedKeys.delete(item.rowKey);
    else expandedKeys.add(item.rowKey);
  }

  function stateDotClass(state: CommentItemState): string {
    switch (state) {
      case 'draft': return 'bg-accent';
      case 'unresolved': return 'bg-warning';
      case 'resolved': return 'bg-success';
      case 'outdated': return 'bg-fg-subtle';
      case 'comment': return 'bg-info';
    }
  }

  function stateLabel(item: CommentListItem): string {
    if (item.state === 'draft') return item.orphaned ? 'draft · orphaned' : 'draft';
    if (item.state === 'comment') return '';
    return item.state;
  }

  function reviewStateLabel(state: string): string {
    return state.toLowerCase().replaceAll('_', ' ');
  }
</script>

<nav class="min-h-0 flex-1 overflow-y-auto pb-2 text-xs" aria-label="Review comments" data-testid="review-comments-list">
  {#if empty}
    <div class="px-3 py-2 text-fg-muted" data-testid="review-comments-empty">No comments yet.</div>
  {/if}

  {#if reviews.length > 0}
    <div class="mt-2 flex items-center gap-1.5 bg-surface-2/50 px-2.5 py-1 text-[0.625rem] font-medium uppercase tracking-[0.08em] text-fg-muted">
      <Icon icon={MessageSquare} size={11} class="shrink-0 opacity-70" />
      Reviews
    </div>
    {#each reviews as review (`${review.authorLogin}:${review.submittedAt}`)}
      <div class="px-2.5 py-1.5" data-testid="review-comments-review">
        <div class="flex items-center gap-1.5">
          <span class="min-w-0 truncate font-medium text-fg">{review.authorLogin}</span>
          <span class="shrink-0 text-fg-muted">{reviewStateLabel(review.state)}</span>
        </div>
        {#if review.body.trim()}
          <div class="mt-1 truncate text-fg-muted">{commentSnippet(review.body)}</div>
        {/if}
      </div>
    {/each}
  {/if}

  {#each groups as group (group.filePath)}
    <!-- Header bands (not bare text) so groups read as sections at a
         glance; group boundaries come from the bands, the separators
         below only divide items within one group. -->
    {#if group.filePath === ''}
      <div class="mt-2 flex items-center gap-1.5 bg-surface-2/50 px-2.5 py-1 text-[0.625rem] font-medium uppercase tracking-[0.08em] text-fg-muted">
        <Icon icon={MessageSquare} size={11} class="shrink-0 opacity-70" />
        Conversation
      </div>
    {:else}
      <div class="mt-2 flex items-center gap-1.5 bg-surface-2/50 px-2.5 py-1 font-mono text-[0.6875rem] text-fg-muted">
        <Icon icon={FileText} size={11} class="shrink-0 opacity-70" />
        <span class="min-w-0 truncate" title={group.filePath}>{group.filePath}</span>
      </div>
    {/if}
    {#each group.items as item (item.rowKey)}
      {@const expanded = expandedKeys.has(item.rowKey)}
      <button
        type="button"
        class="block w-full px-2.5 py-2 text-left hover:bg-surface-2/50"
        data-testid="review-comments-item"
        data-row-key={item.rowKey}
        aria-expanded={item.inDiff ? undefined : expanded}
        onclick={() => activate(item)}
      >
        {#if item.snippet}
          <div class="line-clamp-2 text-fg">{item.snippet}</div>
        {/if}
        <div class="flex items-center gap-1.5 text-[0.625rem] text-fg-subtle {item.snippet ? 'mt-1' : ''}">
          <span class="h-1.5 w-1.5 shrink-0 rounded-full {stateDotClass(item.state)}"></span>
          {#if item.line !== null}
            <span class="shrink-0 tabular-nums">L{item.line}</span>
          {/if}
          <span class="min-w-0 truncate" title={item.author}>{item.author}</span>
          <span class="min-w-0 flex-1"></span>
          {#if item.createdAtMs !== null}
            <span class="shrink-0" data-testid="review-comments-item-time">{relativeTime(item.createdAtMs)}</span>
          {/if}
          {#if item.replies > 0}
            <span class="shrink-0 tabular-nums">+{item.replies}</span>
          {/if}
          {#if stateLabel(item)}
            <span class="shrink-0">{stateLabel(item)}</span>
          {/if}
        </div>
      </button>
      {#if expanded}
        <!-- Sibling of the row button, not a child: the rendered
             markdown carries links, and interactive content must not
             nest inside a button. -->
        <div class="mx-2.5 mb-2 space-y-2 border-l border-border-subtle pl-2" data-testid="review-comments-item-body">
          {#each item.comments as comment, index (index)}
            <div>
              <div class="text-[0.625rem] font-medium text-fg-muted">{comment.author}</div>
              <ChatMarkdown source={comment.body} pathRefs={[]} />
            </div>
          {/each}
        </div>
      {/if}
      <div class="mx-2.5 border-t-2 border-border" role="presentation"></div>
    {/each}
  {/each}
</nav>
