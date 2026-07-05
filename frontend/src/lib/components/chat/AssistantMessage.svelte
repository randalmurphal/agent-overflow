<script lang="ts">
  import type { Item } from '../../types/models';
  import { paneWorkspacePath, type ThreadPane } from '../../stores/thread.svelte';
  import ChatMarkdown from './ChatMarkdown.svelte';
  import CopyButton from '../primitives/CopyButton.svelte';
  import { addToast } from '../../stores/toast.svelte';
  import { getPathRefsFromMeta } from '../../utils/pathLinkify';
  import { formatTimeOfDay } from '../../utils/format';
  import { getSettings } from '../../stores/settings.svelte';
  import { splitAtBoundary } from '../../markdown/boundary/split';

  let { pane, item }: { pane?: ThreadPane; item: Item } = $props();

  const streaming = $derived(item.status === 'streaming');
  const time = $derived(formatTimeOfDay(item.createdAt));
  const isoTime = $derived(new Date(item.createdAt).toISOString());

  // /\S/.test short-circuits at the first non-whitespace character — for
  // long streaming summaries this matters; .trim() would allocate the
  // full string every reactive tick.
  const canCopy = $derived(!streaming && /\S/.test(item.summary));

  // The timestamp/copy row is a completion artifact — it must not float
  // above an EMPTY body. With live updates disabled the body is withheld
  // until the first markdown block commits (ChatMarkdown hides the
  // volatile tail), so an unconditional meta row shows a lone timestamp
  // the instant streaming starts, before any text appears. Gate the row
  // on "is there visible body content yet?".
  //
  // Computed synchronously at mount so settled rows (and streaming rows
  // scrolled back into view with content already committed) show their
  // meta immediately — a deferred $effect would blank the timestamp for
  // one frame on every virtualized remount. The $effect then only latches
  // the false→true transition as content first commits mid-stream; once
  // latched it never re-splits (summary is append-only), so the boundary
  // scan runs only during the brief pre-commit window, never over the
  // whole settled message every tick.
  function bodyIsVisible(): boolean {
    if (!/\S/.test(item.summary)) return false;
    // Settled rows and live-streaming rows reveal as soon as any
    // non-whitespace arrives; only the streaming-disabled path withholds
    // the tail and needs a committed block before anything is on screen.
    if (!streaming || getSettings().streamingEnabled) return true;
    return /\S/.test(splitAtBoundary(item.summary).prefix);
  }
  let hasVisibleBody = $state(bodyIsVisible());
  $effect(() => {
    if (!hasVisibleBody && bodyIsVisible()) hasVisibleBody = true;
  });

  // Path-link allowlist for assistant prose. The settle hook in
  // `internal/triage/stream_state.go` writes a `pathRefs` array onto
  // `Item.meta` for every assistant_text row; here we surface it to
  // ChatMarkdown so only Go-validated paths get wrapped. Falling back
  // to `[]` (not `undefined`) for rows without pathRefs is
  // intentional: pre-pathlinks history rows render plain rather than
  // falling through to the local regex, which is the bug we're fixing.
  // While the row is streaming, settle hasn't fired yet and meta is
  // empty — leave it empty until completion. After settle, meta gains
  // pathRefs and the $derived reads them.
  const pathRefs = $derived(getPathRefsFromMeta(item.meta) ?? []);
</script>

<div class="group" data-item-kind={item.kind}>
  <div
    class="text-fg-muted"
    data-testid="assistant-message-body"
    data-render-mode="client-markdown"
  >
    <ChatMarkdown
      source={item.summary}
      {streaming}
      workspacePath={paneWorkspacePath(pane)}
      {pathRefs}
    />
  </div>
  {#if hasVisibleBody}
    <div
      class="mt-1.5 flex items-center gap-1.5 text-[0.625rem] text-fg-hint/70"
      data-testid="assistant-message-meta"
    >
      <time class="tabular-nums" datetime={isoTime}>
        {time}
      </time>
      <span
        data-testid="assistant-message-copy-slot"
        class="flex h-7 w-7 shrink-0 items-center justify-center"
      >
        {#if canCopy}
          <span class="opacity-0 transition-opacity duration-150 group-hover:opacity-100 focus-within:opacity-100">
            <CopyButton
              text={item.summary}
              label="Copy message"
              onError={() => addToast('error', 'Failed to copy')}
            />
          </span>
        {/if}
      </span>
    </div>
  {/if}
</div>
