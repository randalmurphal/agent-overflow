<script lang="ts">
  // A sprint's form at its control (docs/specs/issues.md, "Backlog and
  // sprint planning"): create a future sprint, change a sprint's name,
  // dates and goal, or start a future sprint, which takes its dates and may
  // change its name and goal in the same request. A change sends only the
  // members changed, and dates once set are changed, not cleared. The write is the board's (`trackerTicketWrites`,
  // `sprintTarget`): while it is out the form takes no change; a refusal
  // stays under it with what was filled; a made write closes it.
  import Button from '../primitives/Button.svelte';
  import Popover from '../primitives/Popover.svelte';
  import TicketWriteNote from './TicketWriteNote.svelte';
  import { restorePickerFocus } from '../panes/paneComposerFocus';
  import { INPUT_CLASS } from '../settings/styles';
  import type { SprintView } from '../../../../bindings/agent-overflow/internal/trackerapp/models';
  import { dayAt, dayValue } from '../../stores/trackerBacklogModel';
  import { createSprint, editSprint, sprintTarget, startSprint, ticketWrite, type BoardWriteContext } from '../../stores/trackerTicketWrites.svelte';
  import type { PopoverCloseReason } from '../../utils/popoverOwnership';
  import { cardFieldName } from './ticketEditors';

  interface Props {
    b: BoardWriteContext;
    mode: 'create' | 'edit' | 'start';
    /** The sprint changed or started; null to create one. */
    sprint: SprintView | null;
    /** The name a new sprint is offered. */
    suggested: string;
    anchor: HTMLElement | undefined;
    onClose: () => void;
  }

  let { b, mode, sprint, suggested, anchor, onClose }: Props = $props();

  const DAY_MS = 86_400_000;
  const target = $derived(sprintTarget(sprint?.id ?? 'new'));
  const pending = $derived(ticketWrite(b.key, target)?.pending ?? false);

  // What the form opened with, which a change is told from. A sprint
  // started now runs from today for as long as it was planned, else two
  // weeks.
  // svelte-ignore state_referenced_locally
  const planned = sprint?.start && sprint.end ? sprint.end - sprint.start : 14 * DAY_MS;
  // svelte-ignore state_referenced_locally
  const from = {
    name: sprint?.name ?? suggested,
    goal: sprint?.goal ?? '',
    start: mode === 'start' ? dayValue(Date.now()) : sprint?.start ? dayValue(sprint.start) : '',
    end: mode === 'start' ? dayValue(Date.now() + planned) : sprint?.end ? dayValue(sprint.end) : '',
  };
  let name = $state(from.name);
  let goal = $state(from.goal);
  let start = $state(from.start);
  let end = $state(from.end);

  const startAt = $derived(dayAt(start, false));
  const endAt = $derived(dayAt(end, true));
  const problem = $derived.by(() => {
    if (!name.trim()) return 'A sprint needs a name.';
    if (mode === 'start' && (startAt === null || endAt === null)) return 'Starting a sprint takes its start and end.';
    // A change sends no date it does not set (SprintEditRequest), so a
    // date once set is changed, never cleared, from here.
    if (mode === 'edit' && ((from.start && startAt === null) || (from.end && endAt === null))) return 'The app cannot clear a sprint\'s dates: change them, or clear them in the tracker.';
    if ((startAt === null) !== (endAt === null)) return 'Give both dates, or neither.';
    if (startAt !== null && endAt !== null && endAt <= startAt) return 'The end comes after the start.';
    return '';
  });
  const changed = $derived(name.trim() !== from.name || goal !== from.goal || start !== from.start || end !== from.end);

  const title = $derived(mode === 'create' ? 'New sprint' : mode === 'start' ? `Start ${sprint?.name ?? 'the sprint'}` : `Change ${sprint?.name ?? 'the sprint'}`);
  const submitLabel = $derived(mode === 'create' ? (pending ? 'Creating…' : 'Create') : mode === 'start' ? (pending ? 'Starting…' : 'Start') : (pending ? 'Saving…' : 'Save'));

  async function submit(): Promise<void> {
    if (problem || pending) return;
    const n = name.trim();
    let sent;
    if (mode === 'create') {
      sent = createSprint(b, { name: n, goal: goal || undefined, start: startAt ?? undefined, end: endAt ?? undefined });
    } else if (mode === 'start') {
      sent = startSprint(b, sprint!.name, {
        sprint: sprint!.id, start: startAt!, end: endAt!,
        name: n !== from.name ? n : undefined, goal: goal !== from.goal ? goal : undefined,
      });
    } else {
      if (!changed) {
        close();
        return;
      }
      sent = editSprint(b, sprint!.name, {
        sprint: sprint!.id,
        name: n !== from.name ? n : undefined,
        goal: goal !== from.goal ? goal : undefined,
        start: start !== from.start ? startAt : undefined,
        end: end !== from.end ? endAt : undefined,
      });
    }
    const outcome = await sent;
    if (outcome?.kind === 'applied' && outcome.note === null) close();
  }

  function close(reason?: PopoverCloseReason): void {
    onClose();
    restorePickerFocus(reason, { triggerEl: anchor });
  }

  const id = $props.id();
</script>

<Popover {anchor} open onClose={close} placement="bottom-end" ariaLabel={title}>
  <form class="flex w-80 max-w-[90vw] flex-col gap-2.5 p-3 text-sm" data-testid="sprint-form" data-mode={mode} onsubmit={(e) => { e.preventDefault(); void submit(); }}>
    <p class="font-medium text-fg">{title}</p>
    <label class="flex flex-col gap-1" for="{id}-name">
      <span class="text-xs text-fg-muted">Name</span>
      <input id="{id}-name" class={INPUT_CLASS} bind:value={name} disabled={pending} autocomplete="off" data-testid="sprint-form-name" />
    </label>
    <div class="flex gap-2">
      <label class="flex min-w-0 flex-1 flex-col gap-1" for="{id}-start">
        <span class="text-xs text-fg-muted">Start</span>
        <input id="{id}-start" type="date" class={INPUT_CLASS} bind:value={start} disabled={pending} data-testid="sprint-form-start" />
      </label>
      <label class="flex min-w-0 flex-1 flex-col gap-1" for="{id}-end">
        <span class="text-xs text-fg-muted">End</span>
        <input id="{id}-end" type="date" class={INPUT_CLASS} bind:value={end} disabled={pending} data-testid="sprint-form-end" />
      </label>
    </div>
    <label class="flex flex-col gap-1" for="{id}-goal">
      <span class="text-xs text-fg-muted">Goal</span>
      <textarea id="{id}-goal" rows="2" class="{INPUT_CLASS} h-auto resize-y py-1.5" bind:value={goal} disabled={pending} data-testid="sprint-form-goal"></textarea>
    </label>
    {#if problem}
      <p class="text-xs text-fg-muted" data-testid="sprint-form-problem">{problem}</p>
    {/if}
    <TicketWriteNote ticketKey={b.key} {target} fieldName={cardFieldName} />
    <div class="flex items-center justify-end gap-1.5">
      <Button size="sm" variant="ghost" disabled={pending} testId="sprint-form-cancel" onclick={() => close()}>Cancel</Button>
      <Button size="sm" variant="primary" type="submit" disabled={pending || !!problem} testId="sprint-form-submit">{submitLabel}</Button>
    </div>
  </form>
</Popover>
