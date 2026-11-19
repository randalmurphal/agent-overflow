<script lang="ts">
  // A small count pill: a tool's "needs you" count in the sidebar's tools
  // row, or a thread row's workers waiting on the user. Renders nothing at
  // zero, so a quiet row shows no badge at all.
  interface Props {
    count: number;
    /** `warning` for work a person must unblock, `accent` for a turn to act. */
    tone: 'warning' | 'accent';
    /** What the number counts, said on hover. */
    title?: string;
    testId?: string;
    class?: string;
  }

  let { count, tone, title, testId, class: className = '' }: Props = $props();

  const TONE: Record<Props['tone'], string> = {
    warning: 'bg-warning/15 text-warning',
    accent: 'bg-accent/15 text-accent',
  };
</script>

{#if count > 0}
  <span
    class={[
      'shrink-0 rounded-full px-1.5 py-px text-[0.6875rem] font-medium leading-tight tabular-nums',
      TONE[tone],
      className,
    ].join(' ')}
    {title}
    data-testid={testId}
    data-tone={tone}
  >{count}</span>
{/if}
