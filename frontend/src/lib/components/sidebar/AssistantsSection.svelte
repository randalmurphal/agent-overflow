<script lang="ts">
  // The Assistants section above the projects (docs/specs/standing-agents.md,
  // "Sidebar"): one row per assistant on every attached computer, under a
  // header that folds the section and carries New Assistant.
  // ProjectsSection buckets what it shows. The rows take no drop and hold
  // no groups.
  import UserRoundPlus from '@lucide/svelte/icons/user-round-plus';
  import type { Thread } from '../../types/models';
  import type { ThreadPane } from '../../stores/thread.svelte';
  import {
    isSidebarSectionShown,
    toggleSidebarSection,
    type SidebarTopLevelList,
  } from '../../stores/sidebar.svelte';

  // The section's rows page under their own key, not a project's entry.
  const ASSISTANTS_LIST: SidebarTopLevelList = { kind: 'assistants' };
  import { openNewAssistantDialog } from '../../stores/newAssistant.svelte';
  import { getProjectLabelText } from '../../stores/projects.svelte';
  import { selectedBackend } from '../../stores/selectedBackend.svelte';
  import { assistantsOffOn, assistantsOn } from '../../stores/assistants.svelte';
  import { scopeGrant } from '../../transport/scopes';
  import { grantBlockedTitle } from '../../transport/scopeRefusal';
  import IconButton from '../primitives/IconButton.svelte';
  import Icon from '../primitives/Icon.svelte';
  import SidebarSectionToggle from './SidebarSectionToggle.svelte';
  import SidebarTreeList from './SidebarTreeList.svelte';
  import { shouldOpenProjectThreadInNewPane } from './projectNewThread';

  interface Props {
    /** The assistants' current threads, already search-filtered. */
    assistants: readonly Thread[];
    pane: ThreadPane | null;
    /** A search is active: the section shows its matches even when folded. */
    searching: boolean;
  }

  let { assistants, pane, searching }: Props = $props();
  let shown = $derived(isSidebarSectionShown('assistants', searching));
  let createGrant = $derived(scopeGrant('threads:autonomy', selectedBackend()));
  // New Assistant creates on the selected computer, which may have its
  // assistants off while another computer's show here.
  let createOff = $derived(!assistantsOn(selectedBackend()));

  // A row's third line: the projects its assistant serves.
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
      title={grantBlockedTitle(createGrant) ?? (createOff ? assistantsOffOn(selectedBackend()) : undefined)}
      size="sm"
      disabled={createGrant !== 'granted' || createOff}
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
      <div class="flex flex-col gap-px rounded-[var(--radius-field)]" role="list" aria-label="Assistants" data-testid="assistant-card-list">
        <SidebarTreeList threads={assistants} list={ASSISTANTS_LIST} {pane} agentProjects={servedProjectsLabel} />
      </div>
    </div>
  {/if}
</section>
