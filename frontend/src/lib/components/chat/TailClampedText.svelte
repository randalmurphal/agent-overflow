<script lang="ts">
  // Shared body primitive for the reasoning-tail rows (ThinkingBlock,
  // CompactionReasoning). While collapsed the body is capped to 3 lines with
  // `overflow: hidden`; the tail-pin effect writes `scrollTop = scrollHeight`
  // so streaming deltas appear at the bottom and older lines scroll off the
  // top. The caller feeds a MONOTONICALLY-GROWING live tail (the per-pane
  // smoother tail), never a re-trimmed sliding window — a shrinking/rewrapping
  // string would make the visible window jump instead of scroll. When expanded
  // the cap is removed and the pin is harmless (scrollHeight === clientHeight).

  let {
    text,
    expanded,
    id,
    testId,
    class: extraClass = '',
  }: {
    text: string;
    expanded: boolean;
    id?: string;
    testId?: string;
    class?: string;
  } = $props();

  let bodyEl: HTMLSpanElement | undefined = $state();

  $effect(() => {
    void text;
    void expanded;
    if (!bodyEl) return;
    bodyEl.scrollTop = bodyEl.scrollHeight;
  });
</script>

<span
  bind:this={bodyEl}
  {id}
  data-testid={testId}
  class={[
    'flex-1 min-w-0 block text-[0.75rem] text-fg-muted/70 italic whitespace-pre-wrap leading-relaxed',
    !expanded ? 'max-h-[3lh] overflow-hidden' : null,
    extraClass || null,
  ]}
>{text}</span>
