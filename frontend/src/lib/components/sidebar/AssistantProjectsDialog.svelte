<script lang="ts">
  import { untrack } from 'svelte';
  // An assistant's Projects dialog (docs/specs/standing-agents.md,
  // "Projects"), from its menu: the projects it serves, or all of them,
  // saved through SetAssistantProjects. The answer's row broadcast updates
  // the card. The dialog follows the assistant, not one of its threads: a
  // handoff while it is open saves to the successor, a rename retitles it,
  // and it closes once the assistant has no current thread in the list.
  import Modal from '../primitives/Modal.svelte';
  import Button from '../primitives/Button.svelte';
  import { SetAssistantProjects } from '../../stores/bindings';
  import { closeAssistantProjectsDialog, getAssistantProjectsAgentId } from '../../stores/newAssistant.svelte';
  import { assistantCurrentThread } from '../../stores/threads.svelte';
  import { requireEntityBackend, withBackendTarget } from '../../transport/backends';
  import { threadBackend } from '../../transport/entityIndex';
  import { threadHasScope } from '../../transport/entityScopes';
  import { userFacingError } from '../../utils/userFacingError';
  import AssistantProjectsPicker from './AssistantProjectsPicker.svelte';

  let agentId = $derived(getAssistantProjectsAgentId());
  let thread = $derived(agentId === null ? undefined : assistantCurrentThread(agentId));
  let all = $state(false);
  let selected: string[] = $state([]);
  let saving = $state(false);
  let error: string | null = $state(null);

  let backend = $derived(thread ? threadBackend(thread.id) : undefined);
  let ungranted = $derived(thread !== undefined && !threadHasScope('threads:autonomy', thread.id));
  let canSave = $derived(thread !== undefined && (all || selected.length > 0) && !ungranted && !saving);

  // Each opening starts from what the assistant serves. Only the opening
  // is tracked: a row frame for the assistant while the dialog is open
  // must not undo the choice being made.
  $effect(() => {
    const opened = agentId;
    if (opened === null) return;
    untrack(() => {
      const current = assistantCurrentThread(opened);
      all = current?.agentAllProjects === true;
      selected = [...(current?.agentProjectIds ?? [])];
      error = null;
      saving = false;
    });
  });

  // Archived, deleted or detached with its computer: nothing to save to.
  $effect(() => {
    if (agentId !== null && thread === undefined) closeAssistantProjectsDialog();
  });

  async function save(): Promise<void> {
    const current = thread;
    if (!current || !canSave) return;
    saving = true;
    error = null;
    try {
      const target = requireEntityBackend(backend);
      await withBackendTarget(target, () => SetAssistantProjects(current.id, all, all ? [] : selected.slice()));
      closeAssistantProjectsDialog();
    } catch (err) {
      console.error('Failed to set the assistant\'s projects:', err);
      error = userFacingError(err);
      saving = false;
    }
  }
</script>

<Modal open={thread !== undefined} title={thread ? `${thread.title}'s projects` : 'Projects'} onClose={closeAssistantProjectsDialog} width="md">
  {#snippet children()}
    <div class="flex flex-col gap-3 text-xs text-fg-muted" data-testid="assistant-projects-dialog">
      <p>The projects this assistant serves. Its next session gets their folders and their memory.</p>
      <AssistantProjectsPicker
        {backend}
        {all}
        {selected}
        disabled={saving}
        onChange={(nextAll, nextSelected) => { all = nextAll; selected = nextSelected; error = null; }}
      />
      {#if error}
        <p role="alert" data-testid="assistant-projects-error" class="rounded-md border border-error/40 bg-error/10 px-3 py-1.5 text-xs text-error">
          {error}
        </p>
      {/if}
    </div>
  {/snippet}
  {#snippet footer()}
    <Button variant="secondary" size="sm" onclick={closeAssistantProjectsDialog} testId="assistant-projects-cancel">
      {#snippet children()}Cancel{/snippet}
    </Button>
    <Button
      variant="primary"
      size="sm"
      onclick={() => void save()}
      disabled={!canSave}
      loading={saving}
      title={ungranted ? 'Not granted to this device' : undefined}
      testId="assistant-projects-save"
    >
      {#snippet children()}{saving ? 'Saving…' : 'Save'}{/snippet}
    </Button>
  {/snippet}
</Modal>
