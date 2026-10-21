<script lang="ts">
  // The Assistants section above the projects (docs/specs/standing-agents.md,
  // "Sidebar"): a card per assistant on every attached computer, holding
  // its helpers and past threads, under a header that folds the section
  // and carries New Assistant. ProjectsSection buckets what it shows. The
  // cards take no drop and hold no groups.
  import UserRoundPlus from '@lucide/svelte/icons/user-round-plus';
  import type { Thread } from '../../types/models';
  import type { ThreadPane } from '../../stores/thread.svelte';
  import { ASSISTANTS_LIST_KEY, isSidebarSectionShown, toggleSidebarSection } from '../../stores/sidebar.svelte';
  import { openNewAssistantDialog } from '../../stores/newAssistant.svelte';
  import { pendingForks } from '../../stores/forkPreparation.svelte';
  import { getProjectLabelText } from '../../stores/projects.svelte';
  import { selectedBackend } from '../../stores/selectedBackend.svelte';
  import { hasScope } from '../../transport/scopes';
  import IconButton from '../primitives/IconButton.svelte';
  import Icon from '../primitives/Icon.svelte';
  import ForkPendingRow from './ForkPendingRow.svelte';
  import SidebarSectionToggle from './SidebarSectionToggle.svelte';
  import SidebarTreeList from './SidebarTreeList.svelte';
  import { shouldOpenProjectThreadInNewPane } from './projectNewThread';

  interface Props {
    /** The cards: assistants' current threads, already search-filtered. */
    assistants: readonly Thread[];
    /** What the cards hold: their helpers and past threads. */
    held: Thread[];
    pane: ThreadPane | null;
    /** A search is active: the section shows its matches even when folded. */
    searching: boolean;
  }

  let { assistants, held, pane, searching }: Props = $props();
  let shown = $derived(isSidebarSectionShown('assistants', searching));
  let createUngranted = $derived(!hasScope('threads:autonomy', selectedBackend()));

  // A card's third line: the projects its assistant serves.
  function servedProjectsLabel(agent: Thread): string {
    if (agent.agentAllProjects) return 'All projects';
    const names = (agent.agentProjectIds ?? []).map((id) => getProjectLabelText(id)).filter((name) => name !== '');
    return names.length > 0 ? names.join(', ') : 'No projects';
  }
</script>

<section aria-label="Assistants" data-testid="sidebar-assistants-section" class="flex flex-col">
  <header class="flex items-center gap-1 px-3 pt-2 pb-1.5">
    <SidebarSectionToggle section="assistants" label="Assistants" {shown} />
    <h2 class="min-w-0 flex-1">
      <button
        type="button"
        onclick={() => toggleSidebarSection('assistants')}
        class="flex items-center text-[0.6875rem] font-medium uppercase tracking-[0.18em] text-fg-subtle select-none hover:text-fg compact:min-h-9 focus-visible:outline-accent"
      >
        Assistants
      </button>
    </h2>
    <IconButton
      label="New Assistant"
      title={createUngranted ? 'Not granted to this device' : undefined}
      size="sm"
      disabled={createUngranted}
      onClick={(e) => openNewAssistantDialog({ targetPane: pane, openInNewPane: shouldOpenProjectThreadInNewPane(e) })}
    >
      {#snippet children()}
        <span data-testid="sidebar-new-assistant-icon" class="flex items-center">
          <Icon icon={UserRoundPlus} size={13} strokeWidth={2.2} class="opacity-80" />
        </span>
      {/snippet}
    </IconButton>
  </header>
  {#if shown && assistants.length > 0}
    <div class="px-2 pb-1">
      <!-- A fork of an assistant's thread or helper lands on its card. -->
      {#each held.filter((thread) => thread.forkPreparing) as thread (thread.id)}
        <ForkPendingRow title={thread.title} />
      {/each}
      {#each pendingForks('').filter((source) => !held.some((thread) => thread.forkPreparing && thread.forkedFromThreadId === source.id)) as source (source.id)}
        <ForkPendingRow title={`${source.title} (fork)`} />
      {/each}
      <div class="flex flex-col gap-px rounded-[var(--radius-field)]" role="list" aria-label="Assistants" data-testid="assistant-card-list">
        <SidebarTreeList threads={held} agents={assistants} scope="" listKey={ASSISTANTS_LIST_KEY} {pane} agentProjects={servedProjectsLabel} />
      </div>
    </div>
  {/if}
</section>
