<script lang="ts">
  import { isLocalEnter } from '../../utils/localEnter';
  import { untrack } from 'svelte';
  // The New Assistant dialog (docs/specs/standing-agents.md, "Sidebar"): a
  // name, the projects it serves (one or more, or all), and Create. The
  // backend creates the record, the home and the thread outside every
  // project, and queues the opening message that starts the discussion of
  // the assistant's role; a name in use is refused inline naming the
  // assistant that holds it.
  import Modal from '../primitives/Modal.svelte';
  import Button from '../primitives/Button.svelte';
  import { CreateAssistant } from '../../stores/bindings';
  import { closeNewAssistantDialog, getNewAssistantRequest, isNewAssistantOpening } from '../../stores/newAssistant.svelte';
  import { assistantsOn } from '../../stores/assistants.svelte';
  import { prependThreadReply } from '../../stores/threads.svelte';
  import { threadRowFrameMark } from '../../stores/threadRowFrames';
  import { getPane, openThreadInNewPane, openThreadInPane } from '../../stores/panes.svelte';
  import { addErrorToast } from '../../stores/toast.svelte';
  import { attachedBackendEntry, backendDisplayName, hasMultipleBackends } from '../../stores/attachedBackends.svelte';
  import { requireEntityBackend, withBackendTarget } from '../../transport/backends';
  import { projectBackend } from '../../transport/entityIndex';
  import { scopeGrant } from '../../transport/scopes';
  import { grantBlockedTitle } from '../../transport/scopeRefusal';
  import type { Thread } from '../../types/models';
  import { userFacingError } from '../../utils/userFacingError';
  import AssistantProjectsPicker from './AssistantProjectsPicker.svelte';

  const INPUT_CLASS = 'min-w-0 flex-1 rounded-[var(--radius-field)] border border-border-subtle bg-surface-1 px-2 py-1.5 text-sm text-fg placeholder:text-fg-hint focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/40';

  let request = $derived(getNewAssistantRequest());
  let name = $state('');
  let all = $state(false);
  let selected: string[] = $state([]);
  let submitting = $state(false);
  let error: string | null = $state(null);

  // The computer the assistant is made on: the project's it came from, or
  // the one selected when the dialog opened from the Assistants header.
  let backend = $derived(request?.projectId ? projectBackend(request.projectId) : request?.selectedBackend);
  let computerName = $derived.by(() => {
    if (!hasMultipleBackends() || backend === undefined) return '';
    const entry = attachedBackendEntry(backend);
    return entry ? backendDisplayName(entry) : '';
  });
  // No computer, no Create: `canCreate` already holds it off without a reason.
  let grant = $derived(backend === undefined ? 'granted' : scopeGrant('threads:autonomy', backend));
  let ungranted = $derived(grant !== 'granted');
  let canCreate = $derived(
    name.trim() !== '' && (all || selected.length > 0) && backend !== undefined && !ungranted && !submitting,
  );

  // Each opening starts clean, on the project the action came from. Only
  // the opening is tracked: the reset reads state it must not re-run on.
  $effect(() => {
    const current = request;
    if (!current) return;
    untrack(() => {
      name = '';
      error = null;
      submitting = false;
      all = false;
      selected = current.projectId ? [current.projectId] : [];
    });
  });

  // Turning assistants off on the computer it creates on closes the
  // dialog; turning them on does not bring it back.
  $effect(() => {
    if (request !== null && backend !== undefined && !assistantsOn(backend)) closeNewAssistantDialog(request);
  });

  function close(): void {
    closeNewAssistantDialog();
  }

  async function create(): Promise<void> {
    const current = request;
    if (!current || !canCreate) return;
    submitting = true;
    error = null;
    const options = { name: name.trim(), projectIds: all ? [] : selected.slice(), allProjects: all };
    let thread: Thread;
    try {
      const target = requireEntityBackend(backend);
      const mark = threadRowFrameMark();
      const reply = (await withBackendTarget(target, () => CreateAssistant(options))) as Thread;
      // The assistant exists whatever became of this opening: the sidebar
      // lists it.
      thread = prependThreadReply(reply, mark);
    } catch (err) {
      console.error('Failed to create an assistant:', err);
      // A later opening's form is not this one's to mark.
      if (!isNewAssistantOpening(current)) return;
      error = userFacingError(err);
      submitting = false;
      return;
    }
    // Cancelled or replaced while it was created: the person is elsewhere.
    if (!isNewAssistantOpening(current)) return;
    closeNewAssistantDialog(current);
    try {
      if (current.openInNewPane) await openThreadInNewPane(thread);
      // A pane closed meanwhile is not revived; the open takes the focused one.
      else await openThreadInPane(thread, current.targetPane && getPane(current.targetPane.paneId) === current.targetPane ? current.targetPane : undefined);
    } catch (err) {
      addErrorToast('The assistant was created, but its thread could not be opened.', err);
    }
  }

  function handleKeydown(e: KeyboardEvent): void {
    if (isLocalEnter(e) && !e.isComposing) {
      e.preventDefault();
      void create();
    }
  }
</script>

<Modal open={request !== null} title="New Assistant" onClose={close} width="md">
  {#snippet children()}
    <div class="flex flex-col gap-3 text-xs text-fg-muted" data-testid="new-assistant-dialog">
      <p>
        An assistant orchestrates work across the projects it serves from a home folder of its own{computerName ? ` on ${computerName}` : ''}. It starts by discussing its role with you.
      </p>
      <label class="flex min-w-0 items-center gap-3">
        <span class="w-20 shrink-0">Name</span>
        <!-- svelte-ignore a11y_autofocus -->
        <input
          type="text"
          class={INPUT_CLASS}
          bind:value={name}
          onkeydown={handleKeydown}
          oninput={() => { error = null; }}
          autofocus
          maxlength="200"
          aria-label="Name"
          aria-invalid={error !== null}
          data-testid="new-assistant-name"
        />
      </label>
      <div class="flex min-w-0 items-start gap-3">
        <span class="w-20 shrink-0 pt-0.5">Serves</span>
        <div class="min-w-0 flex-1">
          <AssistantProjectsPicker
            {backend}
            {all}
            {selected}
            disabled={submitting}
            onChange={(nextAll, nextSelected) => { all = nextAll; selected = nextSelected; error = null; }}
          />
        </div>
      </div>
      {#if error}
        <p
          role="alert"
          data-testid="new-assistant-error"
          class="rounded-md border border-error/40 bg-error/10 px-3 py-1.5 text-xs text-error"
        >
          {error}
        </p>
      {/if}
    </div>
  {/snippet}
  {#snippet footer()}
    <Button variant="secondary" size="sm" onclick={close} testId="new-assistant-cancel">
      {#snippet children()}Cancel{/snippet}
    </Button>
    <Button
      variant="primary"
      size="sm"
      onclick={() => void create()}
      disabled={!canCreate}
      loading={submitting}
      title={grantBlockedTitle(grant)}
      testId="new-assistant-create"
    >
      {#snippet children()}{submitting ? 'Creating…' : 'Create'}{/snippet}
    </Button>
  {/snippet}
</Modal>
