<script lang="ts">
  // Trailing badges for a non-editing thread row: discussion / design
  // indicators. Fork lineage lives in ThreadRow's left metadata
  // cluster because it behaves like a navigation affordance, not a badge.
  //
  // Design threads carry a small monochrome `palette` icon — quiet, not loud —
  // so a glance down the sidebar lets the user pick out the design threads
  // among chat ones without reading text. Discussion rows keep colored text
  // pills since they represent parent / participant lineage.

  import Palette from 'lucide-svelte/icons/palette';
  import Icon from '../primitives/Icon.svelte';
  import type { Thread } from '../../types/models';

  let {
    thread,
  }: {
    thread: Thread;
  } = $props();
</script>

{#if thread.mode === 'discussion'}
  <span class="text-[8.5px] px-1 py-[1px] rounded-[4px] bg-accent/10 text-accent shrink-0" title="Discussion Parent Thread" aria-label="Discussion Parent Thread">D</span>
{:else if thread.parentThreadId}
  <span class="text-[8.5px] px-1 py-[1px] rounded-[4px] bg-provider-codex/10 text-provider-codex shrink-0" title="Discussion Participant" aria-label="Discussion Participant">Dp</span>
{:else if thread.mode === 'design'}
  <span class="shrink-0 inline-flex items-center text-fg-hint" title="Design Thread" aria-label="Design Thread">
    <Icon icon={Palette} size={12} strokeWidth={1.6} />
  </span>
{/if}
