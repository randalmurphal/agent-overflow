<script lang="ts">
  // A Backlog section's sprint actions, on a board whose tracker takes
  // sprint writes (Jira; Linear runs its cycles from the team's settings):
  // Complete sprint and Change on an active sprint, Start sprint and Change
  // on a future one, New sprint on the backlog. Each opens its form at the
  // button, and waits while that sprint's write is out.
  import Button from '../primitives/Button.svelte';
  import SprintCompleteForm from './SprintCompleteForm.svelte';
  import SprintForm from './SprintForm.svelte';
  import type { SprintView } from '../../../../bindings/agent-overflow/internal/trackerapp/models';
  import { nextSprintName, type BacklogSection, type MovedRow } from '../../stores/trackerBacklogModel';
  import type { HeldBoard } from '../../stores/trackerBoardModel';
  import { sprintTarget, ticketWrite, type BoardWriteContext } from '../../stores/trackerTicketWrites.svelte';

  interface Props {
    b: BoardWriteContext;
    held: HeldBoard;
    sec: BacklogSection;
    /** The rows counted in another section than the one holding them. */
    moved: readonly MovedRow[];
    unit: string;
    /** Every sprint of the board, in its order. */
    sprints: readonly SprintView[];
  }

  let { b, held, sec, moved, unit, sprints }: Props = $props();

  type Open = 'create' | 'edit' | 'start' | 'complete';
  let open = $state<Open | null>(null);
  let anchor = $state<HTMLElement | undefined>();

  const pending = $derived(ticketWrite(b.key, sprintTarget(sec.sprint?.id ?? 'new'))?.pending ?? false);
  const into = $derived(sprints.filter((sp) => sp.id !== sec.sprint?.id && (sp.state === 'future' || sp.state === 'active')));

  function show(next: Open, event: MouseEvent): void {
    anchor = event.currentTarget as HTMLElement;
    open = next;
  }
</script>

<span class="flex shrink-0 items-center gap-1" data-testid="issues-backlog-sprint-controls">
  {#if sec.kind === 'active'}
    <Button size="xs" variant="ghost" disabled={pending} testId="issues-backlog-sprint-edit" onclick={(e) => show('edit', e)}>Change</Button>
    <Button size="xs" variant="secondary" disabled={pending} testId="issues-backlog-sprint-complete" onclick={(e) => show('complete', e)}>Complete sprint</Button>
  {:else if sec.kind === 'future'}
    <Button size="xs" variant="ghost" disabled={pending} testId="issues-backlog-sprint-edit" onclick={(e) => show('edit', e)}>Change</Button>
    <Button size="xs" variant="secondary" disabled={pending} testId="issues-backlog-sprint-start" onclick={(e) => show('start', e)}>Start sprint</Button>
  {:else}
    <Button size="xs" variant="secondary" disabled={pending} testId="issues-backlog-sprint-new" onclick={(e) => show('create', e)}>New sprint</Button>
  {/if}
</span>

{#if open === 'complete'}
  <SprintCompleteForm {b} {held} {sec} {moved} {unit} {into} {anchor} onClose={() => { open = null; }} />
{:else if open !== null}
  {@const mode = open}
  <SprintForm {b} {mode} sprint={mode === 'create' ? null : sec.sprint} suggested={nextSprintName(sprints)} {anchor} onClose={() => { open = null; }} />
{/if}
