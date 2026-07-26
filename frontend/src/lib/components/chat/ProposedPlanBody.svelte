<script lang="ts">
  import type { PathRef } from '../../types/models';
  import ChatMarkdown from './ChatMarkdown.svelte';
  import { nestedScroll } from '../../utils/scroll/wheelAttribution';

  interface Props {
    markdown: string;
    capped?: boolean;
    loading?: boolean;
    error?: string | null;
    /** Absolute base directory for resolving relative file paths the
     *  linkifier finds in the plan markdown. */
    workspacePath?: string;
    /** Go-validated path allowlist stamped on the proposed_plan item's
     *  meta at handleProposedPlan time. Empty = no linkification. */
    pathRefs?: PathRef[];
  }

  let {
    markdown,
    capped = false,
    loading = false,
    error = null,
    workspacePath = '',
    pathRefs = [],
  }: Props = $props();
</script>

<div class="mt-4">
  <!-- Registered unconditionally even though the overflow is conditional:
       attribution is decided from live geometry, and an uncapped body has
       scrollHeight === clientHeight, so it can never claim a gesture. -->
  <div
    class:h-96={capped}
    class:overflow-y-auto={capped}
    class="relative border-l-2 border-accent pl-4"
    aria-busy={loading ? 'true' : undefined}
    use:nestedScroll
  >
    <ChatMarkdown source={markdown} {workspacePath} {pathRefs} />
    {#if error}
      <p class="mt-2 text-xs text-error" role="alert">
        Failed to load full plan.
      </p>
    {/if}
  </div>
</div>
