<script lang="ts" module>
  import type { Component } from 'svelte';
  import type { LucideProps } from '@lucide/svelte';
  import type { AttentionKind } from '../../types/models';
  import CircleQuestionMark from '@lucide/svelte/icons/circle-question-mark';
  import Eye from '@lucide/svelte/icons/eye';
  import TriangleAlert from '@lucide/svelte/icons/triangle-alert';

  const KIND_ICONS: Record<AttentionKind, Component<LucideProps>> = { decision: CircleQuestionMark, review: Eye, problem: TriangleAlert };
</script>

<script lang="ts">
  // The line under a thread row naming the attention flag its agent raised
  // (thread_attention): the kind and the agent's reason, until the user
  // reads the thread. The dot above takes the kind's colour; this line says
  // why. The row renders it only while the flag is pending.
  import type { ThreadAttention } from '../../types/models';
  import { ATTENTION_KINDS, attentionKindOf } from '../../utils/threadStatusPill';
  import Icon from '../primitives/Icon.svelte';

  interface Props {
    attention: ThreadAttention;
    /** Left edge of the line, in px. */
    indentPx: number;
  }

  let { attention, indentPx }: Props = $props();

  let kind = $derived(attentionKindOf(attention));
  let shown = $derived(ATTENTION_KINDS[kind]);
</script>

<div
  class="relative flex h-3.5 items-center gap-1 pr-1 text-[0.625rem] leading-none {shown.textClass}"
  style="padding-left: {indentPx}px"
  title="{shown.label}: {attention.reason}"
  data-testid="thread-row-attention"
  data-kind={kind}
>
  <Icon icon={KIND_ICONS[kind]} size={10} strokeWidth={1.8} class="shrink-0" />
  <span class="shrink-0 font-medium">{shown.name}</span>
  <span class="shrink-0 opacity-70" aria-hidden="true">·</span>
  <span class="min-w-0 truncate text-fg-subtle" data-testid="thread-row-attention-reason">{attention.reason}</span>
</div>
