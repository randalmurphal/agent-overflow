<script lang="ts">
  // Workspace diff toggle for the chat header: shows the working-tree churn
  // (+insertions / -deletions) and toggles the right-side diff panel onto its
  // workspace tab.
  //
  // The +/- counts are ALWAYS rendered — +0 -0 when the tree is clean, before
  // any status has been observed, or when the workspace isn't a repo — so the
  // control reads as "no uncommitted changes" rather than empty. The green/red
  // numerals are the entire affordance; there is no leading icon. Ghost-styled,
  // so there's no resting border/pill — just the hover highlight (plus the
  // pressed tint while the panel is open), matching the sibling header toggles.
  //
  // Presentational: status, pressed, chord, and the activate action are passed in.
  import type { GitStatus } from '../../types/git';
  import Button from '../primitives/Button.svelte';

  interface Props {
    status: GitStatus | null;
    pressed: boolean;
    chord: string;
    onActivate: () => void;
  }

  let { status, pressed, chord, onActivate }: Props = $props();

  let insertions = $derived(status?.insertions ?? 0);
  let deletions = $derived(status?.deletions ?? 0);
  let ariaLabel = $derived(
    `Toggle diff panel — ${insertions} insertions, ${deletions} deletions`,
  );
</script>

<Button
  variant="ghost"
  size="xs"
  {pressed}
  {ariaLabel}
  title={`Toggle Diff Panel (${chord})`}
  onclick={onActivate}
  testId="diff-panel-toggle"
  class="shrink-0"
>
  {#snippet children()}
    <span class="flex gap-1 tabular-nums" data-testid="workspace-diff-counts">
      <span class="text-success">+{insertions}</span>
      <span class="text-error">-{deletions}</span>
    </span>
  {/snippet}
</Button>
