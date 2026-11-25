<script lang="ts">
  // One ticket (docs/specs/issues.md, "Ticket"), loaded on its first open
  // in its own chunk. The header holds the type mark, key, parent and
  // title, with Back or Close, over or beside at 900 or wider, Open in the
  // tracker, Refresh and the ticket's menu (also on a right-click or long
  // press of the header). At 720 or wider the content is on the left and
  // the rail of fields on the right; narrower, one row of status,
  // assignee, priority and points sits over the content and Details
  // slides the rail over it. The content is the description, the other
  // long text fields in one line that names them until opened, the linked
  // tickets by relation, each opening in this ticket's place, and the
  // activity: comments and history.
  //
  // What it draws is the ticket's subscription (`attachment`): until its
  // computer has read the ticket's body, the card it was opened from. A
  // computer not connected, a connection turned off, a ticket the tracker
  // does not return or could not be read, each says so in one sentence
  // where the content would be, and nothing else says it. Every change goes through the write store, which keeps its outcome
  // under the control that made it.
  import ArrowLeft from '@lucide/svelte/icons/arrow-left';
  import ChevronDown from '@lucide/svelte/icons/chevron-down';
  import ChevronRight from '@lucide/svelte/icons/chevron-right';
  import Ellipsis from '@lucide/svelte/icons/ellipsis';
  import ExternalLink from '@lucide/svelte/icons/external-link';
  import PanelRight from '@lucide/svelte/icons/panel-right';
  import RefreshCw from '@lucide/svelte/icons/refresh-cw';
  import X from '@lucide/svelte/icons/x';
  import { tick } from 'svelte';
  import Button from '../primitives/Button.svelte';
  import ContextMenu from '../primitives/ContextMenu.svelte';
  import Icon from '../primitives/Icon.svelte';
  import IconButton from '../primitives/IconButton.svelte';
  import MenuDivider from '../primitives/MenuDivider.svelte';
  import MenuItem from '../primitives/MenuItem.svelte';
  import MenuSectionHeader from '../primitives/MenuSectionHeader.svelte';
  import Segmented from '../primitives/Segmented.svelte';
  import ErrorNotice from '../shared/ErrorNotice.svelte';
  import RenderBoundary from '../shared/RenderBoundary.svelte';
  import PriorityIcon from './PriorityIcon.svelte';
  import StatusMark from './StatusMark.svelte';
  import TicketComments from './TicketComments.svelte';
  import TicketField, { type FieldEditor } from './TicketField.svelte';
  import TicketHistory from './TicketHistory.svelte';
  import TicketText from './TicketText.svelte';
  import TicketWriteNote from './TicketWriteNote.svelte';
  import TypeIcon from './TypeIcon.svelte';
  import type { PickItem } from './TicketPicker.svelte';
  import { formatEstimate } from './boardFormat';
  import type {
    CardView,
    FieldEditInput,
    FieldValueView,
    FieldView,
    MoveView,
  } from '../../../../bindings/agent-overflow/internal/trackerapp/models';
  import { captureError, type CapturedError } from '../../stores/errorReports.svelte';
  import {
    setTicketCommentOrder,
    setTicketSectionOpen,
    ticketCommentOrder,
    ticketSectionOpen,
    type TicketPlacement,
    type TicketSeed,
  } from '../../stores/issuesPane.svelte';
  import { addToast } from '../../stores/toast.svelte';
  import {
    dismissTrackerTicketRefreshNotice,
    refreshTrackerTicket,
    type TrackerTicketAttachment,
  } from '../../stores/trackerTicket.svelte';
  import { fieldText, isFilled, linkGroups, moreFields, sectionFields, ticketRow } from '../../stores/trackerTicketModel';
  import {
    assignableUsers,
    editTicketFields,
    fieldChoices,
    fieldTarget,
    moveTicket,
    setTicketPoints,
    ticketWrite,
    type TicketWriteContext,
  } from '../../stores/trackerTicketWrites.svelte';
  import { copyToClipboard } from '../../utils/clipboard';
  import { handleExternalURL } from '../../utils/externalLinks';
  import { relativeTime } from '../../utils/format';

  interface Props {
    attachment: TrackerTicketAttachment;
    seed: TicketSeed;
    /** The board the ticket was opened from: its points are that row's. */
    board: string;
    /** The connection's tracker: jira or linear. */
    kind: string;
    computerName: string;
    /** The ticket's computer is not connected now. */
    offline: boolean;
    compact: boolean;
    /** Tickets Back steps through before it closes this one. */
    trail: number;
    placement: TicketPlacement;
    /** The pane fits the ticket beside the board. */
    besideFits: boolean;
    onPlacement: (placement: TicketPlacement) => void;
    onBack: () => void;
    onClose: () => void;
    /** Opens another issue of the connection from this ticket. */
    onOpenIssue: (issue: string, seed: TicketSeed) => void;
  }

  let { attachment, seed, board, kind, computerName, offline, compact, trail, placement, besideFits, onPlacement, onBack, onClose, onOpenIssue }: Props = $props();

  const NARROW_PX = 720;

  const ticketState = $derived(attachment.current);
  const detail = $derived(ticketState?.detail ?? null);
  const card: CardView | null = $derived(detail?.card ?? seed.card);
  const linear = $derived(kind === 'linear');
  const ctx: TicketWriteContext = $derived({ key: attachment.key, ref: attachment.ref, label: card?.key || seed.key });
  const section = (id: string) => ({ backend: attachment.ref.backend, connection: attachment.ref.connection, section: id });

  // The targets of the narrow row's fields, whose notes the row shows.
  const CARD_TARGETS = ['status', fieldTarget('assignee'), fieldTarget('priority'), 'points'] as const;

  let width = $state(0);
  const narrow = $derived(width > 0 && width < NARROW_PX);
  let railOpen = $state(false);
  $effect(() => {
    if (!narrow) railOpen = false;
  });

  /** One step back inside the ticket: the Details rail over a narrow
   * ticket closes. False when there is none, and the pane steps on. */
  export function stepBack(): boolean {
    if (!railOpen) return false;
    railOpen = false;
    return true;
  }

  // The names fields go by, for a refusal naming them.
  const CARD_NAMES: Readonly<Record<string, string>> = {
    status: 'Status', assignee: 'Assignee', priority: 'Priority', type: 'Type', points: 'Points', description: 'Description', title: 'Title',
  };
  function fieldName(id: string): string {
    return detail?.fields.find((f) => f.id === id)?.name ?? CARD_NAMES[id] ?? id;
  }

  // Picks and typed values, as the writes send them.
  function pickValue(kindName: string, items: readonly { id: string }[]): FieldValueView {
    return items.length === 0 ? {} as FieldValueView : { kind: kindName, choices: items.map((i) => ({ id: i.id })) } as FieldValueView;
  }

  function choicesOf(load: (text: string) => Promise<{ choices: { id: string; name?: string }[]; more?: boolean }>) {
    return async (text: string) => {
      const got = await load(text);
      return { items: (got.choices ?? []).map((c) => ({ id: c.id, name: c.name || c.id })), more: got.more ?? false };
    };
  }

  function editField(field: string, edit: FieldEditInput): void {
    void editTicketFields(ctx, fieldTarget(field), [edit]);
  }

  // The status: the moves the view holds, no tracker call. A move whose
  // screen takes fields opens their form first.
  let moveForm = $state<{ move: MoveView; values: Map<string, FieldEditInput> } | null>(null);
  const statusEditor: FieldEditor | null = $derived(detail ? {
    kind: 'pick',
    checked: new Set<string>(),
    load: async () => ({
      items: detail.moves.map((m) => ({ id: m.id, name: m.to.name || m.name, description: m.name && m.name !== m.to.name ? m.name : undefined })),
      more: false,
    }),
    onPick: (item: PickItem | null) => {
      const move = item && detail.moves.find((m) => m.id === item.id);
      if (!move) return;
      if (move.fields?.length) moveForm = { move, values: new Map() };
      else void moveTicket(ctx, move.id);
    },
  } : null);

  function submitMove(): void {
    const form = moveForm;
    if (!form) return;
    moveForm = null;
    void moveTicket(ctx, form.move.id, [...form.values.values()]);
  }

  const assigneeEditor: FieldEditor | null = $derived(detail ? {
    kind: 'pick',
    clearLabel: 'Unassigned',
    checked: new Set(card?.assignee ? [card.assignee.id] : []),
    load: async (text: string) => {
      const got = await assignableUsers(ctx, text);
      return { items: (got.users ?? []).map((u) => ({ id: u.id, name: u.name || u.id })), more: got.more ?? false };
    },
    onPick: (item: PickItem | null) => editField('assignee', { field: 'assignee', value: pickValue('user', item ? [item] : []) }),
  } : null);

  function choiceEditor(field: string, current: readonly { id: string }[], opts: { clear?: string; multi?: boolean; kindName: string }): FieldEditor {
    return {
      kind: 'pick',
      clearLabel: opts.clear,
      multi: opts.multi,
      checked: new Set(current.map((c) => c.id)),
      load: choicesOf((text) => fieldChoices(ctx, field, text)),
      onPick: (item) => {
        let next: readonly { id: string }[] = item ? [item] : [];
        if (opts.multi && item) next = current.some((c) => c.id === item.id) ? current.filter((c) => c.id !== item.id) : [...current, item];
        editField(field, { field, value: pickValue(opts.kindName, next) });
      },
    };
  }

  const priorityEditor: FieldEditor | null = $derived(detail ? choiceEditor('priority', card?.priority ? [card.priority] : [], { kindName: 'option' }) : null);
  const typeEditor: FieldEditor | null = $derived(detail && !linear ? choiceEditor('type', card?.type ? [card.type] : [], { kindName: 'option' }) : null);

  // Points are the row of the board the ticket was opened from, else the
  // first board the mirror shows it on.
  const row = $derived(ticketRow(detail?.rows, board));
  const pointsLabel = $derived(row?.unit === 'time' ? 'Estimate (hours)' : 'Points');
  const pointsEditor: FieldEditor | null = $derived(row && row.unit !== 'none' ? {
    kind: 'type',
    input: 'number',
    text: row.estimate == null ? '' : String(row.unit === 'time' ? row.estimate / 3600 : row.estimate),
    onSave: (text: string) => {
      const n = text === '' ? null : Number(text);
      void setTicketPoints(ctx, row.board, n === null ? null : row.unit === 'time' ? Math.round(n * 3600) : n);
    },
  } : null);
  const pointsText = $derived(row ? formatEstimate(row.estimate, row.unit) ?? '' : '');

  // A field of the ticket's own: changed in place when it takes values a
  // person picks or types one at a time; a date and time, a read-only
  // value or several typed values (which one line of text cannot hold
  // apart) is shown.
  function editorOf(f: FieldView): FieldEditor | null {
    if (!f.editable) return null;
    switch (f.kind) {
      case 'option':
      case 'user':
        return choiceEditor(f.id, f.value.choices ?? [], { kindName: f.kind, multi: f.list, clear: f.required ? undefined : 'None' });
      case 'text':
      case 'number':
      case 'date':
        if (f.list) return null;
        return {
          kind: 'type',
          input: f.kind,
          text: fieldText(f.value),
          onSave: (text) => editField(f.id, text ? { field: f.id, typed: [text] } : { field: f.id, value: {} as FieldValueView }),
        };
      default:
        return null;
    }
  }

  // Fields in the move form collect the values the move sends.
  function formEditorOf(f: FieldView): FieldEditor | null {
    const base = editorOf({ ...f, editable: true });
    if (!base || !moveForm) return base;
    const form = moveForm;
    const put = (edit: FieldEditInput) => {
      form.values.set(f.id, edit);
      moveForm = { ...form };
    };
    if (base.kind === 'type') return { ...base, onSave: (text) => put(text ? { field: f.id, typed: [text] } : { field: f.id, value: {} as FieldValueView }) };
    return { ...base, onPick: (item) => put({ field: f.id, value: pickValue(f.kind, item ? [item] : []) }) };
  }

  function formValueText(f: FieldView): string {
    const edit = moveForm?.values.get(f.id);
    if (!edit) return fieldText(f.value);
    return edit.typed?.join(', ') ?? (edit.value?.choices?.length ? 'Picked' : '');
  }

  const sections = $derived(detail ? sectionFields(detail.fields) : []);
  const links = $derived(detail ? linkGroups(detail.links ?? []) : []);
  const moreOpen = $derived(ticketSectionOpen(section('more-sections'), false));
  const fieldsOpen = $derived(ticketSectionOpen(section('more-fields'), false));
  let fieldFind = $state('');
  const others = $derived(detail ? moreFields(detail.fields, fieldFind) : []);
  const otherCount = $derived(detail ? moreFields(detail.fields, '').length : 0);
  const filledCount = $derived(detail ? moreFields(detail.fields, '').filter((f) => isFilled(f.value)).length : 0);

  function saveText(field: string) {
    return (markdown: string, from: string) => editTicketFields(ctx, fieldTarget(field), [{ field, markdown: { markdown, from } }]);
  }

  let activity = $state<'comments' | 'history'>('comments');
  const order = $derived(ticketCommentOrder());

  // Controls the menu reaches.
  let statusField: TicketField | undefined = $state();
  let assigneeField: TicketField | undefined = $state();
  let priorityField: TicketField | undefined = $state();
  let pointsField: TicketField | undefined = $state();
  let statusRow: TicketField | undefined = $state();
  let assigneeRow: TicketField | undefined = $state();
  let priorityRow: TicketField | undefined = $state();
  let pointsRow: TicketField | undefined = $state();
  let typeField: TicketField | undefined = $state();
  let descriptionText: TicketText | undefined = $state();
  let comments: TicketComments | undefined = $state();

  async function change(which: 'status' | 'assignee' | 'priority' | 'points' | 'type'): Promise<void> {
    if (narrow && which !== 'type') {
      ({ status: statusRow, assignee: assigneeRow, priority: priorityRow, points: pointsRow })[which]?.edit();
      return;
    }
    if (narrow) {
      railOpen = true;
      await tick();
    }
    ({ status: statusField, assignee: assigneeField, priority: priorityField, points: pointsField, type: typeField })[which]?.edit();
  }

  async function addComment(): Promise<void> {
    activity = 'comments';
    await tick();
    comments?.focusBox();
  }

  // The app's one opener of a web link: this browser for a page served
  // to another machine, the computer's browser otherwise.
  async function openInTracker(): Promise<void> {
    const url = detail?.url;
    if (url && !(await handleExternalURL(url))) addToast('error', `Could not open ${ctx.label} in the tracker: its link ${url} is not a web address.`);
  }

  async function copy(text: string, what: string): Promise<void> {
    if (!(await copyToClipboard(text))) addToast('error', `Could not copy the ${what}.`);
  }

  function refresh(): void {
    void refreshTrackerTicket(attachment.key);
  }

  let menu = $state<{ x: number; y: number } | null>(null);
  let menuButton: HTMLElement | undefined = $state();
  function openMenu(event: MouseEvent): void {
    event.preventDefault();
    menu = { x: event.clientX, y: event.clientY };
  }
  function openMenuFromButton(): void {
    const rect = menuButton?.getBoundingClientRect();
    menu = rect ? { x: rect.left, y: rect.bottom + 4 } : { x: 0, y: 0 };
  }
  function run(action: () => void | Promise<void>): void {
    menu = null;
    void action();
  }

  // Captured once per failure, so Details and copy describe the one shown.
  let refreshFailure: CapturedError | null = $state(null);
  $effect(() => {
    const notice = ticketState?.refreshNotice;
    refreshFailure = notice?.kind === 'failed' ? captureError(notice.error, { context: 'Could not refresh the ticket' }) : null;
  });
  let readFailure: CapturedError | null = $state(null);
  let readFailureOf: unknown = null;
  $effect(() => {
    const err = ticketState?.readError ?? null;
    if (err === readFailureOf) return;
    readFailureOf = err;
    readFailure = err ? captureError(err, { context: "Could not read the ticket from this computer's copy" }) : null;
  });

  const notFound = $derived(ticketState?.failure?.kind === 'not_found');

  function lastRead(at: number): string {
    return at > 0 ? `Last read ${relativeTime(at, 'locale', attachment.ref.backend)}.` : 'Not read yet.';
  }

  // Under compact the pane's header holds Back, and it closes the ticket.
  const backShown = $derived(!compact && trail > 0);
</script>

{#snippet cardFields(rail: boolean)}
  <TicketField bind:this={() => (rail ? statusField : statusRow), (v) => { if (rail) statusField = v; else statusRow = v; }} label="Status" field="status" value={card?.status.name ?? ''} editor={statusEditor} ticketKey={ctx.key} target="status" {fieldName} inline={!rail}>
    {#snippet mark()}<StatusMark group={card?.status.group} />{/snippet}
  </TicketField>
  <TicketField bind:this={() => (rail ? assigneeField : assigneeRow), (v) => { if (rail) assigneeField = v; else assigneeRow = v; }} label="Assignee" field="assignee" value={card?.assignee ? card.assignee.name || 'Unknown user' : ''} editor={assigneeEditor} ticketKey={ctx.key} target={fieldTarget('assignee')} {fieldName} inline={!rail} />
  <TicketField bind:this={() => (rail ? priorityField : priorityRow), (v) => { if (rail) priorityField = v; else priorityRow = v; }} label="Priority" field="priority" value={card?.priority?.name ?? ''} editor={priorityEditor} ticketKey={ctx.key} target={fieldTarget('priority')} {fieldName} inline={!rail}>
    {#snippet mark()}<PriorityIcon priority={card?.priority} testId="ticket-priority-mark" />{/snippet}
  </TicketField>
  {#if row && row.unit !== 'none'}
    <TicketField bind:this={() => (rail ? pointsField : pointsRow), (v) => { if (rail) pointsField = v; else pointsRow = v; }} label={pointsLabel} field="points" value={pointsText} editor={pointsEditor} ticketKey={ctx.key} target="points" {fieldName} inline={!rail} />
  {/if}
{/snippet}

{#snippet railBody()}
  <div class="flex flex-col gap-0.5" data-testid="ticket-fields">
    {#if !linear}
      <TicketField bind:this={typeField} label="Type" field="type" value={card?.type?.name ?? ''} editor={typeEditor} ticketKey={ctx.key} target={fieldTarget('type')} {fieldName}>
        {#snippet mark()}<TypeIcon type={card?.type} testId="ticket-type-mark-rail" />{/snippet}
      </TicketField>
    {/if}
    {@render cardFields(true)}
    {#if card?.sprints?.length}
      <TicketField label="Sprint" field="sprint" value={card.sprints.join(', ')} editor={null} ticketKey={ctx.key} target="sprint" {fieldName} />
    {/if}
    {#if card?.parent}
      <TicketField label="Parent" field="parent" value={card.parent.key ? `${card.parent.key} ${card.parent.title ?? ''}` : card.parent.title ?? ''} editor={null} ticketKey={ctx.key} target="parent" {fieldName} />
    {/if}
    {#if card?.labels?.length}
      <TicketField label="Labels" field="labels" value={card.labels.map((l) => l.name || l.id).join(', ')} editor={null} ticketKey={ctx.key} target="labels" {fieldName} />
    {/if}
    {#if detail && otherCount > 0}
      <button
        type="button"
        class="mt-2 inline-flex items-center gap-1 self-start rounded-[var(--radius-control)] px-1 text-xs text-fg-muted hover:bg-surface-1 hover:text-fg compact:min-h-9 compact:select-none"
        aria-expanded={fieldsOpen}
        data-testid="ticket-more-fields"
        onclick={() => setTicketSectionOpen(section('more-fields'), !fieldsOpen)}
      >
        <Icon icon={fieldsOpen ? ChevronDown : ChevronRight} size={12} strokeWidth={2} />
        {otherCount} more {otherCount === 1 ? 'field' : 'fields'}, {filledCount} filled
      </button>
      {#if fieldsOpen}
        <input
          class="my-1 rounded-[var(--radius-field)] border border-border-subtle bg-surface-0 px-2 py-1 text-sm text-fg focus:outline-none focus-visible:ring-2 focus-visible:ring-accent/40"
          placeholder="Find a field"
          aria-label="Find a field"
          data-testid="ticket-field-find"
          bind:value={fieldFind}
        />
        {#each others as f (f.id)}
          <TicketField label={f.name} field={f.id} value={fieldText(f.value)} editor={editorOf(f)} ticketKey={ctx.key} target={fieldTarget(f.id)} {fieldName} />
        {:else}
          <p class="text-xs text-fg-muted">No field matches.</p>
        {/each}
      {/if}
    {/if}
  </div>
{/snippet}

{#snippet content()}
  {#if moveForm}
    {@const form = moveForm}
    <section class="flex flex-col gap-1.5 rounded-[var(--radius-field)] border border-border-subtle p-2.5" data-testid="ticket-move-form">
      <p class="text-sm text-fg">Moving to {form.move.to.name || form.move.name} takes these fields:</p>
      {#each form.move.fields ?? [] as f (f.id)}
        <TicketField label={f.required ? `${f.name} (required)` : f.name} field={f.id} value={formValueText(f)} editor={formEditorOf(f)} ticketKey={ctx.key} target="move-form" {fieldName} />
      {/each}
      <div class="flex items-center gap-1.5">
        <Button size="sm" variant="primary" testId="ticket-move-submit" onclick={submitMove}>Move</Button>
        <Button size="sm" variant="ghost" onclick={() => { moveForm = null; }}>Cancel</Button>
      </div>
    </section>
  {/if}
  {#if narrow}
    <div class="flex flex-col gap-0.5 border-b border-border-subtle pb-2" data-testid="ticket-narrow-row">
      <div class="flex min-w-0 items-center gap-1">
        {@render cardFields(false)}
      </div>
      {#each CARD_TARGETS as target (target)}
        <TicketWriteNote ticketKey={ctx.key} {target} {fieldName} />
      {/each}
    </div>
  {/if}
  {#if detail}
    <TicketText
      bind:this={descriptionText}
      field="description"
      title="Description"
      markdown={detail.description}
      editable={true}
      open={null}
      onSave={saveText('description')}
      ticketKey={ctx.key}
      target={fieldTarget('description')}
      {fieldName}
    />
    {#if sections.length > 0}
      <div class="flex flex-col gap-3">
        <button
          type="button"
          class="inline-flex min-w-0 items-center gap-1 self-start rounded-[var(--radius-control)] px-1 text-sm text-fg-muted hover:bg-surface-1 hover:text-fg compact:min-h-9 compact:select-none"
          aria-expanded={moreOpen}
          data-testid="ticket-more-sections"
          onclick={() => setTicketSectionOpen(section('more-sections'), !moreOpen)}
        >
          <Icon icon={moreOpen ? ChevronDown : ChevronRight} size={13} strokeWidth={2} />
          <span class="truncate">{sections.length} more {sections.length === 1 ? 'section' : 'sections'}: {sections.map((f) => f.name).join(', ')}</span>
        </button>
        {#if moreOpen}
          {#each sections as f (f.id)}
            {@const id = section(f.id)}
            <TicketText
              field={f.id}
              title={f.name}
              markdown={fieldText(f.value)}
              editable={f.editable ?? false}
              open={ticketSectionOpen(id, true)}
              onFold={(open) => setTicketSectionOpen(id, open)}
              onSave={saveText(f.id)}
              ticketKey={ctx.key}
              target={fieldTarget(f.id)}
              {fieldName}
            />
          {/each}
        {/if}
      </div>
    {/if}
    {#if links.length > 0}
      <section class="flex flex-col gap-1.5" data-testid="ticket-links">
        <span class="px-1 text-sm font-medium text-fg">Linked tickets</span>
        {#each links as group (group.relation)}
          <div class="flex flex-col" data-testid="ticket-link-group">
            <span class="px-1 text-xs text-fg-muted" data-testid="ticket-link-relation">{group.relation}</span>
            {#each group.links as link (link.id)}
              {@const issue = link.issue}
              <button
                type="button"
                class="flex min-w-0 items-center gap-1.5 rounded-[var(--radius-control)] px-1 py-0.5 text-left text-sm text-fg hover:bg-surface-1 focus:outline-none focus-visible:ring-2 focus-visible:ring-accent/40 compact:min-h-9 compact:select-none"
                title="Open {issue.key || issue.title}"
                data-testid="ticket-link"
                onclick={() => onOpenIssue(issue.id, { key: issue.key, title: issue.title, card: null })}
              >
                <StatusMark group={issue.status.group} />
                <span class="shrink-0 tabular-nums text-fg-muted">{issue.key}</span>
                <span class="min-w-0 truncate">{issue.title}</span>
              </button>
            {/each}
          </div>
        {/each}
      </section>
    {/if}
    <section class="flex flex-col gap-2" data-testid="ticket-activity">
      <div class="flex items-center gap-2">
        <span class="text-sm font-medium text-fg">Activity</span>
        <Segmented
          ariaLabel="Activity"
          size="sm"
          value={activity}
          onChange={(next) => { activity = next; }}
          options={[{ value: 'comments', label: 'Comments' }, { value: 'history', label: 'History' }]}
        />
      </div>
      {#if activity === 'comments'}
        <TicketComments bind:this={comments} {ctx} {detail} earlier={ticketState!.earlier} {order} onOrder={setTicketCommentOrder} {fieldName} />
      {:else}
        <TicketHistory {detail} backend={attachment.ref.backend} />
      {/if}
    </section>
  {/if}
{/snippet}

<div class="relative flex h-full min-h-0 min-w-0 flex-col bg-surface-0" bind:clientWidth={width} data-testid="ticket-screen" data-issue={attachment.ref.issue} data-narrow={narrow ? '' : undefined}>
  <!-- svelte-ignore a11y_no_static_element_interactions -->
  <header class="flex flex-col gap-1 border-b border-border-subtle px-3 py-2" oncontextmenu={openMenu} data-testid="ticket-header">
    <div class="flex min-w-0 items-center gap-1.5">
      {#if backShown}
        <IconButton label="Back" size="sm" testId="ticket-back" onClick={onBack}>
          {#snippet children()}<Icon icon={ArrowLeft} size={14} strokeWidth={2} />{/snippet}
        </IconButton>
      {/if}
      {#if !linear}
        <TypeIcon type={card?.type} testId="ticket-type-mark" />
      {/if}
      <span class="shrink-0 text-sm tabular-nums text-fg-muted" data-testid="ticket-key">{card?.key || seed.key}</span>
      {#if card?.parent}
        {@const parent = card.parent}
        <button
          type="button"
          class="min-w-0 truncate rounded-sm bg-accent/10 px-1.5 text-[0.6875rem] text-accent hover:bg-accent/20 compact:min-h-9 compact:select-none"
          title="Open {parent.key || 'the parent'}"
          data-testid="ticket-parent"
          onclick={() => onOpenIssue(parent.id, { key: parent.key ?? '', title: parent.title ?? '', card: null })}
        >{parent.key ? `${parent.key} ${parent.title ?? ''}` : parent.title}</button>
      {/if}
      <span class="flex-1"></span>
      {#if besideFits}
        <Segmented
          ariaLabel="Show the ticket"
          size="sm"
          value={placement}
          onChange={onPlacement}
          options={[{ value: 'over', label: 'Over' }, { value: 'beside', label: 'Beside' }]}
        />
      {/if}
      {#if detail?.url}
        <IconButton label="Open in the tracker" size="sm" testId="ticket-open-tracker" onClick={() => { void openInTracker(); }}>
          {#snippet children()}<Icon icon={ExternalLink} size={13} strokeWidth={2} />{/snippet}
        </IconButton>
      {/if}
      <IconButton label="Refresh the ticket" size="sm" testId="ticket-refresh" disabled={!ticketState || ticketState.ended} onClick={refresh}>
        {#snippet children()}<Icon icon={RefreshCw} size={13} strokeWidth={1.75} class={ticketState?.refreshing ? 'animate-spin' : ''} />{/snippet}
      </IconButton>
      <span bind:this={menuButton} class="inline-flex">
        <IconButton label="Ticket actions" size="sm" testId="ticket-menu-button" ariaHaspopup="menu" onClick={openMenuFromButton}>
          {#snippet children()}<Icon icon={Ellipsis} size={14} strokeWidth={2} />{/snippet}
        </IconButton>
      </span>
      {#if !compact}
        <IconButton label="Close the ticket" size="sm" testId="ticket-close" onClick={onClose}>
          {#snippet children()}<Icon icon={X} size={14} strokeWidth={2} />{/snippet}
        </IconButton>
      {/if}
    </div>
    <h2 class="text-base font-medium leading-snug text-fg [overflow-wrap:anywhere]" data-testid="ticket-title">{card?.title || seed.title}</h2>
    {#if narrow && detail}
      <button type="button" class="inline-flex items-center gap-1 self-start rounded-[var(--radius-control)] px-1.5 text-xs text-fg-muted hover:bg-surface-1 hover:text-fg compact:min-h-9 compact:select-none" aria-expanded={railOpen} data-testid="ticket-details" onclick={() => { railOpen = !railOpen; }}>
        <Icon icon={PanelRight} size={12} strokeWidth={2} />Details
      </button>
    {/if}
  </header>

  {#if detail && ticketState?.ended}
    <p class="border-b border-border-subtle bg-surface-1 px-3 py-1.5 text-xs text-fg-muted" data-testid="ticket-ended">This ticket is no longer followed: its connection was turned off or removed. What shows is as it was last read.</p>
  {:else if detail && ticketState?.failure}
    <p class="border-b border-warning/30 bg-warning/10 px-3 py-1.5 text-xs text-warning" role="status" data-testid="ticket-failure">{ticketState.failure.message} {lastRead(ticketState.readAt)}</p>
  {/if}
  {#if attachment.error}
    <p class="border-b border-error/30 bg-error/10 px-3 py-1.5 text-xs text-error" role="alert" data-testid="ticket-subscribe-error">Could not open this ticket: {attachment.error}</p>
  {/if}
  {#if ticketState?.refreshNotice && ticketState.refreshNotice.kind !== 'failed'}
    <div class="flex items-center gap-2 border-b border-border-subtle bg-surface-1 px-3 py-1.5 text-xs text-fg-muted" role="status" data-testid="ticket-refresh-notice">
      <span class="min-w-0 flex-1">{ticketState.refreshNotice.message}</span>
      {#if ticketState.refreshNotice.kind === 'not_run'}
        <button type="button" class="shrink-0 text-accent hover:underline" onclick={refresh}>Refresh again</button>
      {/if}
      <IconButton label="Dismiss" size="xs" variant="tint" onClick={() => dismissTrackerTicketRefreshNotice(attachment.key)}>
        {#snippet children()}<Icon icon={X} size={12} strokeWidth={2.25} />{/snippet}
      </IconButton>
    </div>
  {/if}
  {#if refreshFailure}
    <ErrorNotice message="Could not refresh the ticket: {refreshFailure.report.reason}" captured={refreshFailure} onDismiss={() => dismissTrackerTicketRefreshNotice(attachment.key)} testId="ticket-refresh-error" />
  {/if}
  {#if readFailure}
    <ErrorNotice message="Could not read the ticket from this computer's copy: {readFailure.report.reason}" captured={readFailure} onDismiss={detail ? () => { readFailure = null; } : undefined} testId="ticket-read-error" />
  {/if}

  <div class="relative flex min-h-0 flex-1">
    <div class="pane-scroll-surface min-h-0 min-w-0 flex-1 overflow-y-auto" data-pane-scroller data-testid="ticket-content">
      <RenderBoundary label="The ticket" testId="ticket-render-error">
        <div class="flex flex-col gap-4 px-3 py-3">
          {#if !detail && notFound}
            <p class="text-sm text-fg-muted" data-testid="ticket-not-found">{ticketState?.failure?.message}</p>
          {:else if !detail && ticketState?.ended}
            <p class="text-sm text-fg-muted" data-testid="ticket-ended-empty">This ticket cannot be shown: its connection was turned off or removed on {computerName}.</p>
          {:else if !detail && offline}
            <p class="text-sm text-fg-muted" role="status" data-testid="ticket-offline">{computerName} is not connected. The ticket shows when it connects.</p>
          {:else if !detail && ticketState?.failure}
            <p class="text-sm text-fg-muted" role="status" data-testid="ticket-failure-empty">{ticketState.failure.message} {lastRead(ticketState.readAt)}</p>
          {:else if !detail && !readFailure && !attachment.error}
            <p class="text-sm text-fg-muted" data-testid="ticket-opening">{ticketState ? 'Reading the ticket from the tracker…' : 'Opening the ticket…'}</p>
          {/if}
          {@render content()}
        </div>
      </RenderBoundary>
    </div>
    {#if !narrow}
      <aside class="w-72 shrink-0 overflow-y-auto border-l border-border-subtle px-3 py-3" data-testid="ticket-rail">
        {@render railBody()}
      </aside>
    {:else if railOpen}
      <aside class="absolute inset-y-0 right-0 z-[2] w-[min(20rem,90%)] overflow-y-auto border-l border-border-subtle bg-surface-0 px-3 py-3 shadow-menu" data-testid="ticket-rail">
        <div class="mb-1 flex items-center">
          <span class="text-sm font-medium text-fg">Details</span>
          <span class="flex-1"></span>
          <IconButton label="Close the details" size="sm" testId="ticket-details-close" onClick={() => { railOpen = false; }}>
            {#snippet children()}<Icon icon={X} size={14} strokeWidth={2} />{/snippet}
          </IconButton>
        </div>
        {@render railBody()}
      </aside>
    {/if}
  </div>
</div>

{#if menu}
  <ContextMenu x={menu.x} y={menu.y} ariaLabel="Ticket actions" onDismiss={() => { menu = null; }}>
    {#if detail}
      <MenuSectionHeader label="Change" />
      <MenuItem label="Status…" disabled={ticketWrite(ctx.key, 'status')?.pending ?? false} testId="ticket-menu-status" onSelect={() => run(() => change('status'))} />
      <MenuItem label="Assignee…" testId="ticket-menu-assignee" onSelect={() => run(() => change('assignee'))} />
      <MenuItem label="Priority…" testId="ticket-menu-priority" onSelect={() => run(() => change('priority'))} />
      {#if row && row.unit !== 'none'}
        <MenuItem label="{pointsLabel}…" testId="ticket-menu-points" onSelect={() => run(() => change('points'))} />
      {/if}
      {#if !linear}
        <MenuItem label="Type…" testId="ticket-menu-type" onSelect={() => run(() => change('type'))} />
      {/if}
      <MenuItem label="Description…" testId="ticket-menu-description" onSelect={() => run(() => descriptionText?.edit())} />
      <MenuDivider />
      <MenuSectionHeader label="Add" />
      <MenuItem label="Comment" testId="ticket-menu-comment" onSelect={() => run(addComment)} />
      <MenuDivider />
    {/if}
    <MenuSectionHeader label="Open" />
    {#if detail?.url}
      <MenuItem label="In the tracker" testId="ticket-menu-open-tracker" onSelect={() => run(openInTracker)} />
    {/if}
    {#if besideFits}
      <MenuItem label="Over the board" checked={placement === 'over'} onSelect={() => run(() => onPlacement('over'))} />
      <MenuItem label="Beside the board" checked={placement === 'beside'} onSelect={() => run(() => onPlacement('beside'))} />
    {/if}
    <MenuItem label="Refresh" testId="ticket-menu-refresh" disabled={!ticketState || ticketState.ended} onSelect={() => run(refresh)} />
    <MenuDivider />
    <MenuSectionHeader label="Copy" />
    <MenuItem label="Key" testId="ticket-menu-copy-key" onSelect={() => run(() => copy(ctx.label, 'key'))} />
    {#if detail?.url}
      {@const url = detail.url}
      <MenuItem label="Link" testId="ticket-menu-copy-link" onSelect={() => run(() => copy(url, 'link'))} />
    {/if}
    <MenuDivider />
    <MenuItem label={trail > 0 ? 'Back' : 'Close'} testId="ticket-menu-close" onSelect={() => run(trail > 0 ? onBack : onClose)} />
  </ContextMenu>
{/if}
