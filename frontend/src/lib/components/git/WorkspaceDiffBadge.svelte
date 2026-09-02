<script lang="ts">
  // Workspace diff toggle for the chat header: shows the working-tree churn
  // (+insertions / -deletions) and toggles the review pane onto workspace
  // scope.
  //
  // The +/- counts are ALWAYS rendered — +0 -0 when the tree is clean, before
  // any status has been observed, or when the workspace isn't a repo — so the
  // control reads as "no uncommitted changes" rather than empty. The green/red
  // numerals are the entire content; there is no leading icon. Secondary
  // (outlined) like every sibling in the header cluster: as a ghost it was the
  // one bare run of text between bordered pills and users read it as a label,
  // not a button, and the pressed tint had no resting chrome to contrast with.
  //
  // Presentational: status, pressed, chord, and the activate action are passed in.
  import type { GitStatus } from '../../types/git';
  import Button from '../primitives/Button.svelte';

  interface Props {
    status: GitStatus | null;
    pressed: boolean;
    /** Display chord for the toggle, or null when the command is unbound. */
    chord: string | null;
    onActivate: () => void;
  }

  let { status, pressed, chord, onActivate }: Props = $props();

  let insertions = $derived(status?.insertions ?? 0);
  let deletions = $derived(status?.deletions ?? 0);
  let ariaLabel = $derived(
    `Toggle review pane — ${insertions} insertions, ${deletions} deletions`,
  );
</script>

<Button
  variant="secondary"
  size="xs"
  {pressed}
  {ariaLabel}
  title={chord ? `Toggle Review Pane (${chord})` : 'Toggle Review Pane'}
  onclick={onActivate}
  testId="review-toggle"
  class="shrink-0"
>
  {#snippet children()}
    <span class="flex gap-1 tabular-nums" data-testid="workspace-diff-counts">
      <span class="text-success">+{insertions}</span>
      <span class="text-error">-{deletions}</span>
    </span>
  {/snippet}
</Button>
