<script lang="ts">
  // Completing an active sprint (docs/specs/issues.md, "Backlog and sprint
  // planning"): the form says how much of it is done and how many of the
  // tickets this board shows are not in a finished status, both by Jira's
  // rule for its completion (`trackerBacklogModel.ts#completionNumbers`: a
  // status of the board's last column that has statuses is finished), not
  // the project's status groups the header counts by, and always asks
  // where the unfinished ones go: an open sprint of the board (the first
  // future one at first) or the backlog. Jira applies that rule to the
  // whole sprint, which can hold tickets this board does not show, with
  // the board's configuration as the write holds it, so the form claims nothing of
  // what will move and sends no destination it did not show. The tracker
  // answers a part per ticket moved and one for the close; a refusal, one
  // of a close the tracker asks to make again since tickets joined the
  // sprint among them, stays under it, and a made completion closes it.
  import Button from '../primitives/Button.svelte';
  import Popover from '../primitives/Popover.svelte';
  import TicketWriteNote from './TicketWriteNote.svelte';
  import { restorePickerFocus } from '../panes/paneComposerFocus';
  import { SELECT_CLASS } from '../settings/styles';
  import type { SprintView } from '../../../../bindings/agent-overflow/internal/trackerapp/models';
  import { completionNumbers, type BacklogSection, type MovedRow } from '../../stores/trackerBacklogModel';
  import type { HeldBoard } from '../../stores/trackerBoardModel';
  import { completeSprint, sprintTarget, ticketWrite, type BoardWriteContext } from '../../stores/trackerTicketWrites.svelte';
  import type { PopoverCloseReason } from '../../utils/popoverOwnership';
  import { sectionLine } from './boardFormat';
  import { cardFieldName } from './ticketEditors';

  interface Props {
    b: BoardWriteContext;
    held: HeldBoard;
    sec: BacklogSection;
    moved: readonly MovedRow[];
    /** The unit the project counts the board in (`BoardShape.counts`). */
    unit: string;
    /** The board's open sprints the unfinished tickets may go to. */
    into: readonly SprintView[];
    anchor: HTMLElement | undefined;
    onClose: () => void;
  }

  let { b, held, sec, moved, unit, into, anchor, onClose }: Props = $props();

  const sprint = $derived(sec.sprint!);
  const target = $derived(sprintTarget(sprint.id));
  const pending = $derived(ticketWrite(b.key, target)?.pending ?? false);
  const completion = $derived(completionNumbers(held, sec, moved));
  const done = $derived(sectionLine('active', completion.numbers, unit).map((p) => p.text).join(''));

  // svelte-ignore state_referenced_locally
  let moveTo = $state(into.find((sp) => sp.state === 'future')?.id ?? '');
  // A sprint picked that a refresh took from the list is not sent: the
  // select keeps its value when its option goes.
  const offered = $derived(moveTo === '' || into.some((sp) => sp.id === moveTo));

  async function submit(): Promise<void> {
    if (pending || !offered) return;
    const outcome = await completeSprint(b, sprint.name, { sprint: sprint.id, move_to: moveTo || undefined });
    if (outcome?.kind === 'applied' && outcome.note === null) close();
  }

  function close(reason?: PopoverCloseReason): void {
    onClose();
    restorePickerFocus(reason, { triggerEl: anchor });
  }

  const id = $props.id();
</script>

<Popover {anchor} open onClose={close} placement="bottom-end" ariaLabel="Complete {sprint.name}">
  <form class="flex w-80 max-w-[90vw] flex-col gap-2.5 p-3 text-sm" data-testid="sprint-complete-form" onsubmit={(e) => { e.preventDefault(); void submit(); }}>
    <p class="font-medium text-fg">Complete {sprint.name}</p>
    <p class="text-fg-muted" data-testid="sprint-complete-summary">
      {done}.
      {#if completion.unfinished !== null}
        {@const unfinished = completion.unfinished}
        {unfinished === 0 ? 'Every ticket this board shows is in a finished status.' : `${unfinished} of the tickets this board shows ${unfinished === 1 ? 'is' : 'are'} not in a finished status.`}
      {:else}
        The sprint is still being read, so its unfinished tickets are not counted yet.
      {/if}
    </p>
    <label class="flex flex-col gap-1" for="{id}-into">
      <span class="text-xs text-fg-muted">The tracker moves every ticket of the sprint it counts as unfinished to</span>
      <select id="{id}-into" class={SELECT_CLASS} bind:value={moveTo} disabled={pending} data-testid="sprint-complete-into">
        {#each into as sp (sp.id)}
          <option value={sp.id}>{sp.name}</option>
        {/each}
        <option value="">The backlog</option>
      </select>
    </label>
    {#if !offered}
      <p class="text-xs text-fg" role="status" data-testid="sprint-complete-gone">That sprint is no longer open. Pick where the unfinished tickets go.</p>
    {/if}
    <TicketWriteNote ticketKey={b.key} {target} fieldName={cardFieldName} />
    <div class="flex items-center justify-end gap-1.5">
      <Button size="sm" variant="ghost" disabled={pending} testId="sprint-complete-cancel" onclick={() => close()}>Cancel</Button>
      <Button size="sm" variant="primary" type="submit" disabled={pending || !offered} testId="sprint-complete-submit">{pending ? 'Completing…' : 'Complete sprint'}</Button>
    </div>
  </form>
</Popover>
