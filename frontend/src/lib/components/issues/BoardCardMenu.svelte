<script lang="ts">
  // A Board card's menu (docs/specs/issues.md, "Board"), drawn by the
  // shared ActionMenu under the name `issues-card`: Open, Open in its own
  // pane (where ticket panes are offered) and Open in the tracker; Move to…, Move to sprint… (a board that plans sprints),
  // Assignee…, Priority…, Points… (a board with an estimate) and Type…
  // (a tracker with issue types); Ask and Start work…, as the ticket's
  // Work runs them (`ticketWorkGate.ts` says why either cannot run);
  // Copy key and Copy link. Opening it sends nothing and loads none of
  // Work's code: Move to… reads the ticket's moves when its page opens, and a
  // field row opens the card's own editor at its mark, which reads what
  // the ticket screen's editor reads. Under compact the status mark opens
  // it at Move to….
  import ActionMenu, { type MenuGroup } from '../shared/ActionMenu.svelte';
  import CardMovePage from './CardMovePage.svelte';
  import MenuItem from '../primitives/MenuItem.svelte';
  import type { MoveView, RowView } from '../../../../bindings/agent-overflow/internal/trackerapp/models';
  import type { BoardShape } from '../../stores/trackerBoardModel';
  import { ticketPanesOffered } from '../../stores/ticketPanes.svelte';
  import { fieldTarget, ticketWrite, type TicketWriteContext } from '../../stores/trackerTicketWrites.svelte';
  import type { CardEditField } from './ticketEditors';
  import { workGate, workProject, workProjects } from './ticketWorkGate';

  interface Props {
    x: number;
    y: number;
    row: RowView;
    ctx: TicketWriteContext;
    shape: BoardShape;
    /** The column the card shows in. */
    from: number;
    /** The tracker has issue types (not Linear). */
    typed: boolean;
    /** The card's address in the tracker (`cardURL`); empty when unknown. */
    url: string;
    /** A move or rank of the card is out. */
    moving: boolean;
    /** Opens at Move to…. */
    atMove?: boolean;
    onOpen: () => void;
    /** Opens its ticket in a pane of its own. */
    onOwnPane: () => void;
    onOpenURL: () => void;
    onCopy: (text: string, what: string) => void;
    onEdit: (field: CardEditField) => void;
    onMove: (column: number, move: MoveView) => void;
    onSprint: (sprint: string | null) => void;
    /** The project the card's ticket's Ask and Start work use; null when
     * none is known. */
    project: string | null;
    /** Ask in `project`. */
    onAsk: (project: string) => void;
    /** Start work: the ticket opens with its form. */
    onStartWork: () => void;
    onDismiss: () => void;
  }

  let { x, y, row, ctx, shape, from, typed, url, moving, atMove = false, onOpen, onOwnPane, onOpenURL, onCopy, onEdit, onMove, onSprint, project, onAsk, onStartWork, onDismiss }: Props = $props();

  /** Shown at once until the person chooses. */
  const DEFAULTS = ['open', 'move', 'assignee', 'copy-key'];

  // The sprints it can move to: the board's active and future ones but
  // its own, and the backlog when it is in a sprint.
  const sprints = $derived(shape.plansSprints ? shape.sprints.filter((sp) => (sp.state === 'active' || sp.state === 'future') && !row.card.sprints?.includes(sp.id)) : []);
  const inSprint = $derived((row.card.sprints?.length ?? 0) > 0);
  // A row whose write is out waits for it; the store would send nothing.
  const pending = (target: string) => ticketWrite(ctx.key, target)?.pending ?? false;

  const threadProject = $derived(workProject(workProjects(ctx.ref.backend, project), project));
  const gate = $derived(workGate(ctx.ref.backend, threadProject));

  const groups = $derived<MenuGroup[]>([
    {
      label: 'Open',
      actions: [
        { id: 'open', label: 'Open', testId: 'issues-card-menu-open', run: onOpen },
        ...(ticketPanesOffered() ? [{ id: 'own-pane', label: 'Open in its own pane', testId: 'issues-card-menu-own-pane', run: onOwnPane }] : []),
        ...(url ? [{ id: 'open-tracker', label: 'Open in the tracker', testId: 'issues-card-menu-open-tracker', run: onOpenURL }] : []),
      ],
    },
    {
      label: 'Change',
      actions: [
        { id: 'move', label: 'Move to…', testId: 'issues-card-menu-move', disabled: moving, page: movePage },
        ...(shape.plansSprints ? [{ id: 'sprint', label: 'Move to sprint…', testId: 'issues-card-menu-sprint', disabled: pending('sprint'), page: sprintPage }] : []),
        { id: 'assignee', label: 'Assignee…', testId: 'issues-card-menu-assignee', disabled: pending(fieldTarget('assignee')), run: () => onEdit('assignee') },
        { id: 'priority', label: 'Priority…', testId: 'issues-card-menu-priority', disabled: pending(fieldTarget('priority')), run: () => onEdit('priority') },
        ...(shape.unit !== 'none' && shape.unit !== '' ? [{ id: 'points', label: shape.unit === 'time' ? 'Estimate…' : 'Points…', testId: 'issues-card-menu-points', disabled: pending('points'), run: () => onEdit('points') }] : []),
        ...(typed ? [{ id: 'type', label: 'Type…', testId: 'issues-card-menu-type', disabled: pending(fieldTarget('type')), run: () => onEdit('type') }] : []),
      ],
    },
    {
      label: 'Threads',
      actions: [
        { id: 'ask', label: 'Ask', description: gate.ask || undefined, disabled: !!gate.ask, testId: 'issues-card-menu-ask', run: () => onAsk(threadProject) },
        { id: 'start-work', label: 'Start work…', description: gate.start || undefined, disabled: !!gate.start, testId: 'issues-card-menu-start-work', run: onStartWork },
      ],
    },
    {
      label: 'Copy',
      actions: [
        { id: 'copy-key', label: 'Copy key', testId: 'issues-card-menu-copy-key', run: () => onCopy(row.card.key, 'key') },
        ...(url ? [{ id: 'copy-link', label: 'Copy link', testId: 'issues-card-menu-copy-link', run: () => onCopy(url, 'link') }] : []),
      ],
    },
  ]);
</script>

{#snippet movePage(controls: { close(): void })}
  <CardMovePage {ctx} columns={shape.columns} {from} onPick={(column, move) => { onMove(column, move); controls.close(); }} />
{/snippet}

{#snippet sprintPage(controls: { close(): void })}
  {#each sprints as sp (sp.id)}
    <MenuItem label={sp.name} description={sp.state === 'active' ? 'Active' : 'Planned'} testId="issues-card-sprint" onSelect={() => { onSprint(sp.id); controls.close(); }} />
  {/each}
  {#if inSprint}
    <MenuItem label="Backlog" testId="issues-card-sprint" onSelect={() => { onSprint(null); controls.close(); }} />
  {/if}
  {#if sprints.length === 0 && !inSprint}
    <MenuItem label="No other sprint is open" disabled testId="issues-card-sprint-none" />
  {/if}
{/snippet}

<ActionMenu {x} {y} ariaLabel="Card actions for {row.card.key}" name="issues-card" defaults={DEFAULTS} {groups} opening={atMove ? 'move' : undefined} {onDismiss} />
