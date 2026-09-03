<script lang="ts">
  import { WorkflowGetJobNotes, WorkflowSetJobNotes } from '../../stores/bindings';
  import { addToast } from '../../stores/toast.svelte';
  import { hasScope } from '../../transport/scopes';
  interface Props { automationId: string }
  let { automationId }: Props = $props();
  let expanded = $state(false);
  let notes = $state('');
  let loaded = $state(false);
  let timer: ReturnType<typeof setTimeout> | null = null;
  // The notes ride an automation's row, which the workflow engine owns.
  let ungranted = $derived(!hasScope('threads:autonomy'));

  $effect(() => {
    if (!ungranted || !timer) return;
    clearTimeout(timer);
    timer = null;
  });

  async function toggle(): Promise<void> {
    expanded = !expanded;
    if (!expanded || loaded) return;
    try { notes = await WorkflowGetJobNotes(automationId); loaded = true; }
    catch { addToast('error', 'Could not load continuity notes'); }
  }

  function saveSoon(): void {
    if (ungranted) return;
    if (timer) clearTimeout(timer);
    timer = setTimeout(async () => {
      timer = null;
      if (ungranted) return;
      try { await WorkflowSetJobNotes(automationId, notes); }
      catch { addToast('error', 'Could not save continuity notes'); }
    }, 350);
  }
</script>

<section class="rounded-lg border border-border-subtle" data-testid="wf-job-notes">
  <button class="w-full px-3 py-2 text-left text-xs text-fg-muted" onclick={toggle} data-testid="wf-job-notes-toggle">{expanded ? '▼' : '▶'} Continuity notes — carried across runs</button>
  {#if expanded}
    <textarea bind:value={notes} oninput={saveSoon} onblur={saveSoon} disabled={ungranted} title={ungranted ? 'Not granted to this device' : undefined} class="m-3 mt-0 min-h-28 w-[calc(100%-1.5rem)] rounded-md border border-border-subtle bg-surface-0 p-2 text-sm disabled:cursor-not-allowed disabled:opacity-50" data-testid="wf-job-notes-input"></textarea>
  {/if}
</section>
