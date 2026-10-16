<script lang="ts">
  /*
   * Notification renderer for the `abandoned_blocks` discriminator: the
   * record that Claude discarded the streamed rows just above it after the
   * response stalled (internal/triage/abandoned_blocks.go). The rows it
   * names stay in the timeline as they streamed and take their muted
   * marking from this record (utils/discardedItems.ts); this row sits at
   * the seam between them and whatever Claude wrote next.
   *
   * Decoded meta shape:
   *   { kind: "abandoned_blocks", title, api_message_id, from_block_index, item_ids }
   *
   * The tail clause reads what followed from the loaded rows rather than
   * promising it: the CLI's own contract says a retry, a resend, an error
   * or nothing can follow.
   */
  import Scissors from '@lucide/svelte/icons/scissors';
  import Icon from '../primitives/Icon.svelte';
  import type { Item } from '../../types/models';
  import type { TimelineSource } from '../../stores/threadPaneRoles';
  import { abandonedItemIds, abandonedWholeResponse, assistantRowAfter } from '../../utils/discardedItems';

  let { pane, item }: { pane?: Pick<TimelineSource, 'items'>; item: Item } = $props();

  const discardedCount = $derived(abandonedItemIds(item).length);
  const whole = $derived(abandonedWholeResponse(item));
  const followed = $derived(pane ? assistantRowAfter(pane.items, item) !== null : false);
  const tail = $derived.by(() => {
    if (!followed) return '';
    return whole ? 'Claude started the reply over below.' : 'Claude picked the reply up below.';
  });
</script>

<div
  class="mb-1.5 flex items-center gap-1.5 px-2 py-1 text-[0.6875rem] italic text-fg-subtle"
  data-testid="abandoned-blocks-notification"
  data-discarded-count={discardedCount}
  data-whole-response={whole ? 'true' : 'false'}
  data-followed={followed ? 'true' : 'false'}
>
  <Icon icon={Scissors} size={11} strokeWidth={2} class="shrink-0 opacity-70" />
  <span>{item.summary || 'Discarded by Claude after a stalled response'}{#if tail}.<span class="text-fg-hint">{' '}{tail}</span>{/if}</span>
</div>
