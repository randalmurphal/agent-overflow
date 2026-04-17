<script lang="ts">
  import type {
    DiscussionDefinition,
    DiscussionParticipant,
    DiscussionScope,
  } from '../../types/discussion';
  import { DEFAULT_MAX_TURNS, createEmptyDiscussionDefinition } from '../../types/discussion';
  import {
    CreateDiscussion,
    UpdateDiscussion,
    DeleteDiscussion,
  } from '../../stores/bindings';
  import { addToast } from '../../stores/toast.svelte';
  import ConfirmDialog from '../shared/ConfirmDialog.svelte';
  import ParticipantForm from './ParticipantForm.svelte';

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
  // Save. When the parent swaps which definition is being edited (prop change),
  // we detect via the `initial` reference and re-seed the draft.
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

  function cloneDef(def: DiscussionDefinition): DiscussionDefinition {
    return {
      ...def,
      participants: def.participants.map((p) => ({ ...p })),
      settings: { ...def.settings },
    };
  }

  function updateField<K extends keyof DiscussionDefinition>(key: K, value: DiscussionDefinition[K]): void {
    draft = { ...draft, [key]: value };
  }

  function updateScope(scope: DiscussionScope): void {
    draft = {
      ...draft,
      scope,
      projectId: scope === 'project' ? draft.projectId ?? '' : '',
    };
  }

  function updateMaxTurns(raw: string): void {
    const parsed = parseInt(raw, 10);
    const maxTurns = Number.isFinite(parsed) && parsed > 0 ? parsed : DEFAULT_MAX_TURNS;
    draft = { ...draft, settings: { ...draft.settings, maxTurns } };
  }

  function updateParticipant(index: number, next: DiscussionParticipant): void {
    draft = {
      ...draft,
      participants: draft.participants.map((p, i) => (i === index ? next : p)),
    };
  }

  function addParticipant(): void {
    draft = {
      ...draft,
      participants: [
        ...draft.participants,
        { role: '', description: '', system: '', provider: undefined, model: undefined },
      ],
    };
  }

  function removeParticipant(index: number): void {
    if (draft.participants.length <= 2) return;
    draft = {
      ...draft,
      participants: draft.participants.filter((_, i) => i !== index),
    };
  }

  /**
   * Mirror of the Go-side `normalizeDiscussionDefinition` validation so we
   * can surface errors inline instead of round-tripping to the backend.
   * Keep these in sync with `internal/discussion/registry.go`.
   */
  function validate(): string | null {
    if (!draft.name.trim()) return 'Discussion name is required.';
    if (draft.participants.length < 2) return 'A discussion needs at least 2 participants.';
    if (draft.scope === 'project' && !(draft.projectId ?? '').trim()) {
      return 'Project-scoped discussions require a project path.';
    }
    for (let i = 0; i < draft.participants.length; i++) {
      const p = draft.participants[i];
      if (!p.role.trim()) return `Participant ${i + 1} needs a role.`;
      if (!p.system.trim()) return `Participant ${i + 1} needs a system prompt.`;
    }
    if (!Number.isInteger(draft.settings.maxTurns) || draft.settings.maxTurns < 1) {
      return 'Max turns must be a positive integer.';
    }
    return null;
  }

  async function handleSubmit(e: Event): Promise<void> {
    e.preventDefault();
    if (saving) return;
    const err = validate();
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
    if (isNew) {
      draft = createEmptyDiscussionDefinition();
    } else {
      draft = cloneDef(initial);
    }
    validationError = null;
  }
</script>

<form onsubmit={handleSubmit} class="space-y-4" aria-label={isNew ? 'Create discussion' : 'Edit discussion'}>
  <section class="rounded-2xl border border-border/70 bg-surface-1/80 p-5 shadow-[0_10px_40px_-24px_rgba(0,0,0,0.45)]">
    <div class="mb-3">
      <p class="text-[11px] font-semibold uppercase tracking-[0.22em] text-text-secondary/70">Definition</p>
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
          required
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

  <section class="rounded-2xl border border-border/70 bg-surface-1/80 p-5 shadow-[0_10px_40px_-24px_rgba(0,0,0,0.45)]">
    <div class="mb-3 flex items-center justify-between gap-3">
      <div>
        <p class="text-[11px] font-semibold uppercase tracking-[0.22em] text-text-secondary/70">Participants</p>
        <h3 class="mt-1 text-base font-semibold text-text-primary">Who is in the room</h3>
      </div>
      <button
        type="button"
        onclick={addParticipant}
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
          onChange={(next) => updateParticipant(i, next)}
          onRemove={() => removeParticipant(i)}
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
      <button
        type="submit"
        disabled={saving}
        class="rounded-xl bg-accent px-4 py-2 text-xs font-semibold text-surface-0 hover:opacity-90 disabled:opacity-40 disabled:cursor-not-allowed cursor-pointer focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/50"
      >
        {saving ? 'Saving...' : isNew ? 'Create' : 'Save changes'}
      </button>
      <button
        type="button"
        onclick={handleReset}
        disabled={saving}
        class="rounded-xl border border-border px-4 py-2 text-xs text-text-secondary hover:text-text-primary cursor-pointer disabled:opacity-40 disabled:cursor-not-allowed focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/50"
      >
        Reset
      </button>
      {#if onCancel}
        <button
          type="button"
          onclick={onCancel}
          class="rounded-xl border border-border px-4 py-2 text-xs text-text-secondary hover:text-text-primary cursor-pointer focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/50"
        >
          Cancel
        </button>
      {/if}
    </div>
    {#if !isNew && onDeleted}
      <button
        type="button"
        onclick={() => { showDeleteConfirm = true; }}
        disabled={deleting}
        class="rounded-xl border border-error/40 bg-error/10 px-4 py-2 text-xs font-medium text-error hover:bg-error/20 disabled:opacity-40 disabled:cursor-not-allowed cursor-pointer focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-error/50"
      >
        Delete discussion
      </button>
    {/if}
  </div>
</form>

<ConfirmDialog
  open={showDeleteConfirm}
  title="Delete discussion"
  description={`This permanently removes the discussion "${initial.name}" from this ${initial.scope === 'project' ? 'project' : 'workstation'}. Running threads that referenced it keep working, but you can no longer start it.`}
  confirmLabel="Delete"
  destructive={true}
  onConfirm={handleDelete}
  onCancel={() => { showDeleteConfirm = false; }}
/>
