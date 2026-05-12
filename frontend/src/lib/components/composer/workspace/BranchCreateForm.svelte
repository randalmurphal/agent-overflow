<script lang="ts">
  // Inline branch-create form rendered above the branch list when the
  // user picks "+ New branch…" from BranchPicker. Owns the form state
  // (name, pending, error) and the create call. Base is $bindable so the
  // parent's branch list can set it when the user clicks a branch row.

  import { tick } from 'svelte';
  import type { ThreadPane } from '../../../stores/thread.svelte';
  import type { Thread } from '../../../types/models';
  import { GitCreateBranchFrom } from '../../../stores/bindings';
  import { errString } from '../../../utils/errors';
  import {
    isLocalBase,
    resolveBaseForWire,
  } from '../../../stores/worktreeIntent.svelte';
  import Button from '../../primitives/Button.svelte';

  interface Props {
    pane: ThreadPane;
    workspaceDirty: boolean;
    currentBranch: string;
    base: string;
    onCancel: () => void;
    onCreated: (thread: Thread) => void;
  }

  let {
    pane,
    workspaceDirty,
    currentBranch,
    base = $bindable(),
    onCancel,
    onCreated,
  }: Props = $props();

  let nameInputEl: HTMLInputElement | undefined = $state(undefined);
  let name = $state('');
  let pending = $state(false);
  let error: string | null = $state(null);

  // The destructive path: dirty workspace + non-Local base. Frontend
  // surfaces a confirm chip; a single button click is the explicit
  // acknowledgement that backend trusts.
  let discardsChanges = $derived(
    workspaceDirty && !isLocalBase(base) && base !== '',
  );

  // Mount-time autofocus. Tick first so the input is in the DOM.
  $effect(() => {
    void (async () => {
      await tick();
      nameInputEl?.focus();
    })();
  });

  function handleKeydown(event: KeyboardEvent): void {
    event.stopPropagation();
    if (event.key === 'Enter') {
      event.preventDefault();
      void submit();
    } else if (event.key === 'Escape') {
      event.preventDefault();
      cancel();
    }
  }

  function cancel(): void {
    name = '';
    error = null;
    pending = false;
    onCancel();
  }

  async function submit(): Promise<void> {
    if (!pane.thread || pending) return;
    const trimmed = name.trim();
    if (!trimmed) {
      error = 'Branch name required';
      return;
    }
    if (!base) {
      error = 'Pick a base';
      return;
    }
    pending = true;
    error = null;
    try {
      const wire = resolveBaseForWire(base, currentBranch);
      const updated = (await GitCreateBranchFrom(
        pane.thread.id,
        trimmed,
        wire.baseBranch,
        wire.carryLocalChanges,
      )) as Thread;
      onCreated(updated);
    } catch (err) {
      console.error('GitCreateBranchFrom failed:', err);
      error = errString(err);
    } finally {
      pending = false;
    }
  }
</script>

<div class="px-2 pb-1 pt-1 space-y-1.5" data-testid="branch-picker-create-form">
  <input
    bind:this={nameInputEl}
    bind:value={name}
    type="text"
    placeholder="New branch name"
    onkeydown={handleKeydown}
    class={[
      'h-7 w-72 rounded border border-border-subtle bg-surface-0',
      'px-2 text-xs text-text-primary placeholder:text-fg-hint',
      'focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/40',
    ].join(' ')}
  />
  <div class="flex items-center gap-2 text-[11px] text-fg-hint">
    <span>From:</span>
    <span class="truncate text-fg">
      {#if isLocalBase(base)}
        Local (with changes)
      {:else if base}
        {base}
      {:else}
        <span class="text-fg-hint">Pick a base below</span>
      {/if}
    </span>
  </div>
  {#if discardsChanges}
    <div class="text-[11px] text-warning" data-testid="branch-picker-create-discards">
      Discards uncommitted changes.
    </div>
  {/if}
  {#if error}
    <div class="text-[11px] text-error truncate">{error}</div>
  {/if}
  <div class="flex items-center justify-end gap-2">
    <Button variant="ghost" size="xs" onclick={cancel} disabled={pending}>
      Cancel
    </Button>
    <Button
      variant={discardsChanges ? 'danger' : 'secondary'}
      size="xs"
      onclick={submit}
      disabled={pending || !name.trim() || !base}
      testId="branch-picker-create-submit"
    >
      {#if pending}
        Creating…
      {:else if discardsChanges}
        Discard and create
      {:else}
        Create
      {/if}
    </Button>
  </div>
</div>
