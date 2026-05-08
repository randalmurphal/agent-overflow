<script lang="ts">
  // Single-line hint rendered immediately under the composer card.
  // Surfaces the UP-arrow retract affordance only when a plain UP would
  // actually retract — otherwise the textarea's native cursor-up runs
  // and the hint would be a lie. The conditions mirror
  // `shouldRetractQueueOnUpArrow` (composerKeyboard.ts):
  //   - active thread
  //   - Zone 1 of the send queue has at least one item
  //   - composer draft is empty (no text, attachments, or terminal chips)
  //
  // Reserved-slot pattern: the wrapper carries a `min-h-*` so toggling
  // visibility never animates the composer's vertical position. The
  // composerOverlay (ChatView.svelte) is bottom-anchored, so the slot's
  // height is reserved beneath the composer card without pushing the
  // textarea down. Same trick ProviderStatusBanner / TransportStatusBanner
  // use upstream — see frontend/CLAUDE.md "Reserved-slot banners".
  //
  // Lives outside the timeline scroll surface; height changes propagate
  // via `--composer-height` (the overlay's ResizeObserver), so toggling
  // the hint also tightens the timeline's bottom padding by the slot's
  // height. The composer's bottom edge is unchanged either way.
  //
  // Wording is intentionally generic ("message(s)") with a dynamic
  // singular/plural choice when we know the count — matches the
  // SendNowButton's understated tone and avoids i18n complexity.
  import type { ThreadPane } from '../../stores/thread.svelte';
  import type { ComposerDraftStore } from '../../stores/composerDraft.svelte';
  import { getQueueForThread } from '../../stores/sendQueue.svelte';

  interface Props {
    pane: ThreadPane;
    draft: ComposerDraftStore;
  }

  let { pane, draft }: Props = $props();

  let queued = $derived(getQueueForThread(pane.threadId ?? ''));
  let isComposerEmpty = $derived(
    draft.content.trim().length === 0
    && draft.attachments.length === 0
    && draft.terminalChips.length === 0,
  );
  let visible = $derived(
    !!pane.thread
    && queued.length > 0
    && isComposerEmpty,
  );
  let label = $derived(
    queued.length === 1
      ? 'Press ↑ to retract queued message'
      : 'Press ↑ to retract queued messages',
  );
</script>

<div
  class="px-6 min-h-[1.25rem]"
  data-testid="composer-hint-slot"
  aria-hidden={!visible}
>
  {#if visible}
    <div
      class="mx-auto flex w-full max-w-[68rem] items-center text-[11px] text-text-secondary/75"
      data-testid="composer-hint"
    >
      {label}
    </div>
  {/if}
</div>
