<script lang="ts">
  import type {
    DiscussionDefinition,
    DiscussionParticipant,
    DiscussionScope,
  } from '../../types/discussion';
  import { createEmptyDiscussionDefinition } from '../../types/discussion';
  import {
    CreateDiscussion,
    UpdateDiscussion,
    DeleteDiscussion,
  } from '../../stores/bindings';
  import { addToast } from '../../stores/toast.svelte';
  import ConfirmDialog from '../shared/ConfirmDialog.svelte';
  import ParticipantForm from './ParticipantForm.svelte';
  import Button from '../primitives/Button.svelte';
  import {
    addParticipant,
    cloneDef,
    removeParticipant,
    setDefField,
    setMaxTurns,
    setScope,
    updateParticipant,
    validateDiscussion,
  } from './discussionEditorHelpers';

  let {
    initial,
    isNew,
    onSaved,
    onDeleted,
    onCancel,
  }: {
    initial: DiscussionDefinition;
    isNew: boolean;
    onSaved: (def: DiscussionDefinition) => void;
    onDeleted?: (def: DiscussionDefinition) => void;
    onCancel?: () => void;
  } = $props();

  // Local editable draft. We deep-clone so parent state isn't mutated until
  // Save. When the parent swaps which definition is being edited (prop
  // change), we detect via the `initial` reference and re-seed the draft.
  // svelte-ignore state_referenced_locally
  let draft = $state<DiscussionDefinition>(cloneDef(initial));
  // svelte-ignore state_referenced_locally
  let previousKey = $state(`${initial.scope}::${initial.name}`);

  $effect(() => {
    const nextKey = `${initial.scope}::${initial.name}`;
    if (nextKey !== previousKey) {
      draft = cloneDef(initial);
      previousKey = nextKey;
      validationError = null;
    }
  });

  let saving = $state(false);
  let deleting = $state(false);
  let showDeleteConfirm = $state(false);
  let validationError = $state<string | null>(null);

  function updateField<K extends keyof DiscussionDefinition>(key: K, value: DiscussionDefinition[K]): void {
    draft = setDefField(draft, key, value);
  }

  function updateScope(scope: DiscussionScope): void {
    draft = setScope(draft, scope);
  }

  function updateMaxTurns(raw: string): void {
    draft = setMaxTurns(draft, raw);
  }

  function updateParticipantAt(index: number, next: DiscussionParticipant): void {
    draft = updateParticipant(draft, index, next);
  }

  function handleAddParticipant(): void {
    draft = addParticipant(draft);
  }

  function handleRemoveParticipant(index: number): void {
    draft = removeParticipant(draft, index);
  }

  async function handleSubmit(e: Event): Promise<void> {
    e.preventDefault();
    if (saving) return;
    const err = validateDiscussion(draft);
    if (err) {
      validationError = err;
      return;
    }
    validationError = null;
    saving = true;
    try {
      if (isNew) {
        await CreateDiscussion(draft);
        addToast('success', `Created discussion "${draft.name}".`);
      } else {
        await UpdateDiscussion(initial.name, initial.scope, draft);
        addToast('success', `Saved discussion "${draft.name}".`);
      }
      onSaved(draft);
    } catch (rpcErr) {
      console.error('Failed to save discussion:', rpcErr);
      addToast('error', `Failed to save discussion: ${rpcErr}`);
    } finally {
      saving = false;
    }
  }

  async function handleDelete(): Promise<void> {
    if (deleting) return;
    deleting = true;
    try {
      await DeleteDiscussion(initial.name, initial.scope);
      addToast('success', `Deleted discussion "${initial.name}".`);
      onDeleted?.(initial);
    } catch (rpcErr) {
      console.error('Failed to delete discussion:', rpcErr);
      addToast('error', `Failed to delete discussion: ${rpcErr}`);
    } finally {
      deleting = false;
      showDeleteConfirm = false;
    }
  }

  function handleReset(): void {
    draft = isNew ? createEmptyDiscussionDefinition() : cloneDef(initial);
    validationError = null;
  }
</script>

<form onsubmit={handleSubmit} class="space-y-4" aria-label={isNew ? 'Create discussion' : 'Edit discussion'}>
  <section class="rounded-[var(--radius-control)] border border-border-subtle bg-card/30 p-5">
    <div class="mb-3">
      <p class="text-[0.6875rem] font-semibold uppercase tracking-[0.22em] text-text-secondary/70">Definition</p>
      <h3 class="mt-1 text-base font-semibold text-text-primary">
        {isNew ? 'New discussion' : `Editing "${initial.name}"`}
      </h3>
    </div>

    <div class="grid grid-cols-1 gap-3 md:grid-cols-2">
      <div>
        <label for="discussion-name" class="block text-xs font-medium text-text-primary mb-1">
          Name <span class="text-error/80">*</span>
        </label>
        <input
          id="discussion-name"
          type="text"
          value={draft.name}
          oninput={(e) => updateField('name', (e.target as HTMLInputElement).value)}
          placeholder="e.g. code-review"
          class="w-full text-sm rounded-xl border border-border bg-surface-0 px-3 py-2 text-text-primary placeholder:text-text-secondary/50 focus:outline-none focus:border-accent focus-visible:ring-2 focus-visible:ring-accent/50 transition-colors"
          aria-required="true"
        />
      </div>

      <div>
        <label for="discussion-scope" class="block text-xs font-medium text-text-primary mb-1">Scope</label>
        <select
          id="discussion-scope"
          value={draft.scope}
          onchange={(e) => updateScope((e.target as HTMLSelectElement).value as DiscussionScope)}
          class="w-full text-sm rounded-xl border border-border bg-surface-0 px-3 py-2 text-text-primary focus:outline-none focus:border-accent focus-visible:ring-2 focus-visible:ring-accent/50 transition-colors cursor-pointer"
        >
          <option value="global">Global</option>
          <option value="project">Project</option>
        </select>
      </div>

      <div class="md:col-span-2">
        <label for="discussion-description" class="block text-xs font-medium text-text-primary mb-1">Description</label>
        <input
          id="discussion-description"
          type="text"
          value={draft.description}
          oninput={(e) => updateField('description', (e.target as HTMLInputElement).value)}
          placeholder="What this discussion is for"
          class="w-full text-sm rounded-xl border border-border bg-surface-0 px-3 py-2 text-text-primary placeholder:text-text-secondary/50 focus:outline-none focus:border-accent focus-visible:ring-2 focus-visible:ring-accent/50 transition-colors"
        />
      </div>

      {#if draft.scope === 'project'}
        <div class="md:col-span-2">
          <label for="discussion-project" class="block text-xs font-medium text-text-primary mb-1">
            Project path <span class="text-error/80">*</span>
          </label>
          <input
            id="discussion-project"
            type="text"
            value={draft.projectId ?? ''}
            oninput={(e) => updateField('projectId', (e.target as HTMLInputElement).value)}
            placeholder="/absolute/path/to/project"
            class="w-full text-sm rounded-xl border border-border bg-surface-0 px-3 py-2 text-text-primary placeholder:text-text-secondary/50 focus:outline-none focus:border-accent focus-visible:ring-2 focus-visible:ring-accent/50 transition-colors font-mono"
          />
        </div>
      {/if}

      <div>
        <label for="discussion-max-turns" class="block text-xs font-medium text-text-primary mb-1">Max turns</label>
        <input
          id="discussion-max-turns"
          type="number"
          min="1"
          value={draft.settings.maxTurns}
          oninput={(e) => updateMaxTurns((e.target as HTMLInputElement).value)}
          class="w-full text-sm rounded-xl border border-border bg-surface-0 px-3 py-2 text-text-primary focus:outline-none focus:border-accent focus-visible:ring-2 focus-visible:ring-accent/50 transition-colors"
        />
      </div>
    </div>
  </section>

  <section class="rounded-[var(--radius-control)] border border-border-subtle bg-card/30 p-5">
    <div class="mb-3 flex items-center justify-between gap-3">
      <div>
        <p class="text-[0.6875rem] font-semibold uppercase tracking-[0.22em] text-text-secondary/70">Participants</p>
        <h3 class="mt-1 text-base font-semibold text-text-primary">Who Is in the Room</h3>
      </div>
      <button
        type="button"
        onclick={handleAddParticipant}
        class="rounded-xl border border-border px-3 py-1.5 text-xs text-text-secondary hover:text-text-primary hover:border-text-secondary cursor-pointer focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/50"
      >
        + Add participant
      </button>
    </div>

    <div class="space-y-3">
      {#each draft.participants as participant, i (i)}
        <ParticipantForm
          participant={participant}
          index={i}
          canRemove={draft.participants.length > 2}
          onChange={(next) => updateParticipantAt(i, next)}
          onRemove={() => handleRemoveParticipant(i)}
        />
      {/each}
    </div>
  </section>

  {#if validationError}
    <div role="alert" aria-live="polite" class="rounded-xl border border-error/40 bg-error/12 px-3 py-2 text-xs text-error">
      {validationError}
    </div>
  {/if}

  <div class="flex flex-wrap items-center justify-between gap-2">
    <div class="flex gap-2">
      <Button
        variant="primary"
        size="md"
        type="submit"
        loading={saving}
      >
        {#snippet children()}{saving ? 'Saving...' : isNew ? 'Create' : 'Save changes'}{/snippet}
      </Button>
      <Button
        variant="secondary"
        size="md"
        onclick={handleReset}
        disabled={saving}
      >
        {#snippet children()}Reset{/snippet}
      </Button>
      {#if onCancel}
        <Button variant="secondary" size="md" onclick={onCancel}>
          {#snippet children()}Cancel{/snippet}
        </Button>
      {/if}
    </div>
    {#if !isNew && onDeleted}
      <Button
        variant="danger-outline"
        size="md"
        onclick={() => { showDeleteConfirm = true; }}
        loading={deleting}
      >
        {#snippet children()}Delete discussion{/snippet}
      </Button>
    {/if}
  </div>
</form>

<ConfirmDialog
  open={showDeleteConfirm}
  title="Delete Discussion"
  description={`This permanently removes the discussion "${initial.name}" from this ${initial.scope === 'project' ? 'project' : 'workstation'}. Running threads that referenced it keep working, but you can no longer start it.`}
  confirmLabel="Delete"
  destructive={true}
  onConfirm={handleDelete}
  onCancel={() => { showDeleteConfirm = false; }}
/>
