<script lang="ts">
  import { REVIEW_COLLAPSED_ROW_PX } from '../../utils/reviewRows';
  import type { PatchFile } from '../../utils/patchFiles';

  // Collapsed-file body row. Same exact-height contract as the file
  // header: rendered height must equal REVIEW_COLLAPSED_ROW_PX.

  interface Props {
    file: PatchFile;
    onExpand: () => void;
  }

  let { file, onExpand }: Props = $props();
</script>

<button
  type="button"
  class="box-border flex w-full cursor-pointer items-center gap-2 border-b border-border-subtle/60 bg-surface-0/40 px-8 text-left text-xs hover:bg-surface-2/30"
  style:height="{REVIEW_COLLAPSED_ROW_PX}px"
  data-testid="review-file-collapsed"
  data-path={file.path}
  onclick={onExpand}
>
  <span class="text-accent">Show diff</span>
  <span class="tabular-nums text-fg-muted">
    {#if file.additions > 0}<span class="text-success">+{file.additions}</span>{/if}
    {#if file.deletions > 0}<span class="text-error">-{file.deletions}</span>{/if}
  </span>
</button>
