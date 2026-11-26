<script lang="ts">
  // The listed boards whose statuses a page cannot list until this
  // computer reads them (trackerapp's SiteStatuses.unread), each said on
  // its own line with Check again, which reads the project's statuses
  // again. Status groups shows a site's, Starting work a tracker
  // project's.
  import type { UnreadBoard } from '../../../../bindings/agent-overflow/internal/trackerapp/models';
  import { refreshTrackerStatuses } from '../../stores/trackerSetup.svelte';
  import { GHOST_BUTTON_CLASS } from './styles';

  let { boards, project }: { boards: readonly UnreadBoard[]; project: string } = $props();
</script>

{#each boards as board (board.board)}
  <div class="flex flex-wrap items-center gap-2" data-testid="tracker-status-unread">
    <p class="text-[0.75rem] text-fg-muted">
      {board.name || `Board ${board.board}`} has not been read on this computer yet. Open it once in the Issues pane to list its statuses.
    </p>
    <button type="button" class={GHOST_BUTTON_CLASS} onclick={() => void refreshTrackerStatuses(project)} data-testid="tracker-status-unread-retry">Check again</button>
  </div>
{/each}
