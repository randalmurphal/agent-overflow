<script lang="ts">
  // One ticket (docs/specs/issues.md, "Ticket"), loaded on its first open
  // in its own chunk. The header holds the type mark (which changes the
  // type), key, parent and title, with Back or Close, over or beside at
  // 900 or wider, Open in the tracker, Refresh and the ticket's menu (also
  // on a right-click or long press of the header). At 720 or wider the content is on the left and
  // the rail of fields on the right; narrower, one row of status,
  // assignee, priority and points sits over the content and Details
  // slides the rail over it. The content is the description, the other
  // long text fields in one line that names them until opened, the linked
  // tickets by relation, each opening in this ticket's place, the
  // attachments, planning poker on a Jira site that has it (folded until
  // opened), and the activity: comments and history. A Jira ticket's
  // header also holds its estimate, which opens the estimate's editor.
  // Files dropped anywhere on a Jira ticket, or pasted with the focus in
  // it (an editor's too, whose text stays the editor's), are attached.
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
  import { setContext, tick, untrack } from 'svelte';
  import { TRACKER_MEDIA_CONTEXT } from '../chat/markdown/trackerMediaContext';
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
  import TicketAttachments from './TicketAttachments.svelte';
  import TicketComments from './TicketComments.svelte';
  import TicketPoker from './TicketPoker.svelte';
  import TicketField, { type FieldEditor } from './TicketField.svelte';
  import TicketHistory from './TicketHistory.svelte';
  import TicketLinks from './TicketLinks.svelte';
  import TicketText from './TicketText.svelte';
  import WorklogForm from './WorklogForm.svelte';
  import Popover from '../primitives/Popover.svelte';
  import TicketWriteNote from './TicketWriteNote.svelte';
  import TypeIcon from './TypeIcon.svelte';
  import TicketMoveForm from './TicketMoveForm.svelte';
  import TicketPicker, { type PickItem } from './TicketPicker.svelte';
  import { fieldEditor, pickEditor, ticketTarget, type EditorTarget } from './ticketEditors';
  import { formatEstimate } from './boardFormat';
  import type {
    CardView,
    FieldEditInput,
    FieldValueView,
    FieldView,
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
    askTrackerPokerSupport,
    dismissTrackerTicketRefreshNotice,
    refreshTrackerTicket,
    trackerPokerSupport,
    type TrackerTicketAttachment,
  } from '../../stores/trackerTicket.svelte';
  import { fieldText, isFilled, moreFields, pokerEstimateChoice, pokerResult, sectionFields, shownText, ticketRow } from '../../stores/trackerTicketModel';
  import { localDay } from '../../stores/trackerTimeModel';
  import {
    assignableUsers,
    attachTicketFiles,
    editTicketFields,
    fieldChoices,
    fieldTarget,
    moveTicket,
    openTicketMove,
    revealTicketPoker,
    setTicketPoints,
    ticketMoveDraft,
    ticketWrite,
    type TicketWriteContext,
  } from '../../stores/trackerTicketWrites.svelte';
  import { copyToClipboard } from '../../utils/clipboard';
  import { handleExternalURL, safeExternalURL } from '../../utils/externalLinks';
  import { clipboardFiles, hasFilePayload } from '../../utils/filePayload';
  import { relativeTime } from '../../utils/format';
  import { trackerMediaReader, type TrackerAttachmentTicket } from '../../utils/trackerAttachments';

  interface Props {
    attachment: TrackerTicketAttachment;
    seed: TicketSeed;
    /** The board the ticket was opened from: its points are that row's. */
    board: string;
    /** The connection's tracker: jira or linear. */
    kind: string;
    computerName: string;
    /** The person's account on the connection; null when it was never
     * verified, and Log time cannot say whose time it logs. */
    account: string | null;
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

  let { attachment, seed, board, kind, computerName, account, offline, compact, trail, placement, besideFits, onPlacement, onBack, onClose, onOpenIssue }: Props = $props();

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

  function editField(field: string, edit: FieldEditInput): void {
    void editTicketFields(ctx, fieldTarget(field), [edit]);
  }

  // The status: the moves the view holds, no tracker call. A move whose
  // screen takes fields opens their form first, each long field starting
  // from the text the ticket shows for it.
  const moveForm = $derived(ticketMoveDraft(ctx.key));
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
      if (!move.fields?.length) {
        void moveTicket(ctx, move.id);
        return;
      }
      const long = move.fields.filter((f) => f.kind === 'rich_text' && f.editable);
      openTicketMove(ctx.key, move, new Map(long.map((f) => [f.id, shownText(detail, f.id)])));
    },
  } : null);

  // A field the ticket holds: its edit is saved at once.
  function targetOf(field: string, value: FieldValueView): EditorTarget {
    return ticketTarget(value, (text) => fieldChoices(ctx, field, text), (edit) => editField(field, edit));
  }
  const choiceValue = (c: { id: string; name?: string } | null | undefined): FieldValueView => (c ? { kind: 'option', choices: [c] } : {}) as FieldValueView;

  const assigneeEditor: FieldEditor | null = $derived(detail ? pickEditor('assignee', 'user', {
    ...targetOf('assignee', choiceValue(card?.assignee)),
    choices: async (text) => {
      const got = await assignableUsers(ctx, text);
      return { choices: (got.users ?? []).map((u) => ({ id: u.id, name: u.name })), more: got.more };
    },
  }, { clear: 'Unassigned' }) : null);
  const priorityEditor: FieldEditor | null = $derived(detail ? pickEditor('priority', 'option', targetOf('priority', choiceValue(card?.priority))) : null);
  const typeEditor: FieldEditor | null = $derived(detail && !linear ? pickEditor('type', 'option', targetOf('type', choiceValue(card?.type))) : null);
  // The header's type mark opens the same picker as the rail's Type row.
  let typeMarkButton: HTMLButtonElement | undefined = $state();
  let typeMarkPicking = $state(false);

  // Points are the row of the board the ticket was opened from, else the
  // first board the mirror shows it on.
  const row = $derived(ticketRow(detail?.rows, board));
  // The row the ticket's estimate is set on: points, or hours typed for a
  // board that estimates in time (the tracker takes seconds).
  const estimateRow = $derived(row && row.unit !== 'none' ? row : null);
  const pointsLabel = $derived(row?.unit === 'time' ? 'Estimate (hours)' : 'Points');
  function setEstimate(on: NonNullable<typeof estimateRow>, value: number | null): void {
    void setTicketPoints(ctx, on.board, value === null ? null : on.unit === 'time' ? Math.round(value * 3600) : value);
  }
  const pointsEditor: FieldEditor | null = $derived(estimateRow ? {
    kind: 'type',
    input: 'number',
    text: estimateRow.estimate == null ? '' : String(estimateRow.unit === 'time' ? estimateRow.estimate / 3600 : estimateRow.estimate),
    onSave: (text: string) => setEstimate(estimateRow, text === '' ? null : Number(text)),
  } : null);
  const pointsText = $derived(row ? formatEstimate(row.estimate, row.unit) ?? '' : '');
  const pointsPending = $derived(ticketWrite(ctx.key, 'points')?.pending ?? false);

  // Open in the tracker and Copy link take a web page's address only.
  const trackerURL = $derived(safeExternalURL(detail?.url));
  const mediaTicket: TrackerAttachmentTicket = $derived({ key: attachment.key, ref: attachment.ref, url: trackerURL ?? '' });
  // The files the ticket's text names show as its attachments.
  setContext(TRACKER_MEDIA_CONTEXT, trackerMediaReader(() => (detail ? { ticket: mediaTicket, attachments: detail.attachments ?? [] } : null)));

  // A tracker that stores files takes them: a pick, a drop or a paste.
  const canAttach = $derived(!linear && !!detail);
  const attachmentsOpen = $derived(ticketSectionOpen(section('attachments'), true));
  let attachmentsSection: TicketAttachments | undefined = $state();
  function attach(files: readonly File[]): void {
    if (files.length === 0) return;
    setTicketSectionOpen(section('attachments'), true);
    attachTicketFiles(ctx, files);
  }
  let dropping = $state(false);
  function onDragOver(event: DragEvent): void {
    if (!canAttach || !hasFilePayload(event)) return;
    event.preventDefault();
    if (event.dataTransfer) event.dataTransfer.dropEffect = 'copy';
    dropping = true;
  }
  function onDragLeave(event: DragEvent): void {
    if (event.relatedTarget instanceof Node && (event.currentTarget as HTMLElement).contains(event.relatedTarget)) return;
    dropping = false;
  }
  function onDrop(event: DragEvent): void {
    dropping = false;
    if (!canAttach || !hasFilePayload(event)) return;
    event.preventDefault();
    attach(Array.from(event.dataTransfer?.files ?? []));
  }
  // A paste's files are attached; its text stays the editor's.
  function onPaste(event: ClipboardEvent): void {
    if (!canAttach) return;
    const files = clipboardFiles(event);
    if (files.length === 0) return;
    if (!event.clipboardData?.types.includes('text/plain')) event.preventDefault();
    attach(files);
  }

  // Planning poker, on a Jira site that has it: the store's poker support
  // rules decide when it is asked. The ticket asks when it shows its body,
  // and again when its computer connected again (`unasked`).
  const hasDetail = $derived(!!detail);
  const pokerSupport = $derived(linear ? null : trackerPokerSupport(attachment.ref.backend, attachment.ref.connection));
  const pokerUnasked = $derived(pokerSupport === 'unasked');
  function askPokerSupport(): void {
    askTrackerPokerSupport(attachment.ref.backend, attachment.ref.connection);
  }
  $effect(() => {
    if (linear || !hasDetail) return;
    void pokerUnasked;
    untrack(askPokerSupport);
  });
  const pokerOffered = $derived(hasDetail && pokerSupport !== null && typeof pokerSupport === 'object' && 'available' in pokerSupport && pokerSupport.available);
  const pokerSupportFailure = $derived(hasDetail && pokerSupport !== null && typeof pokerSupport === 'object' && 'failed' in pokerSupport ? pokerSupport.failed : null);
  const pokerOpen = $derived(ticketSectionOpen(section('poker'), false));
  const pokerPending = $derived(ticketWrite(ctx.key, 'poker')?.pending ?? false);
  // A revealed round's values, which the menu offers as the estimate as
  // the section does; known only while the section follows the votes.
  const pokerView = $derived(ticketState?.poker?.view ?? null);
  const pokerEstimates = $derived(pokerView?.loaded && pokerView.revealed ? pokerResult(pokerView.votes ?? []) : []);
  let pokerBody: TicketPoker | undefined = $state();
  async function openPoker(then: (body: TicketPoker) => void): Promise<void> {
    setTicketSectionOpen(section('poker'), true);
    await tick();
    if (pokerBody) then(pokerBody);
  }
  function revealVotes(): void {
    setTicketSectionOpen(section('poker'), true);
    void revealTicketPoker(ctx);
  }

  // Log time: the Time screen's form, filled with this ticket and today.
  let logging = $state(false);
  const timeCtx = $derived(account ? { backend: attachment.ref.backend, connection: attachment.ref.connection, author: account, computer: computerName } : null);

  // A field of the ticket's own, changed in place.
  const editorOf = (f: FieldView): FieldEditor | null => fieldEditor(f, targetOf(f.id, f.value));

  const sections = $derived(detail ? sectionFields(detail.fields) : []);
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
  let linksSection: TicketLinks | undefined = $state();

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
    if (!trackerURL || !(await handleExternalURL(trackerURL))) addToast('error', `Could not open ${ctx.label} in the tracker: it has no web address.`);
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
  {#if moveForm && detail}
    <TicketMoveForm {ctx} form={moveForm} fields={detail.fields} {fieldName} onOpenTracker={() => { void openInTracker(); }} />
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
      markdown={shownText(detail, 'description')}
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
              markdown={shownText(detail, f.id)}
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
    <TicketLinks bind:this={linksSection} {ctx} links={detail.links ?? []} {onOpenIssue} />
    {#if !linear || (detail.attachments?.length ?? 0) > 0}
      <TicketAttachments
        bind:this={attachmentsSection}
        {ctx}
        ticket={mediaTicket}
        attachments={detail.attachments ?? []}
        {canAttach}
        open={attachmentsOpen}
        onFold={(open) => setTicketSectionOpen(section('attachments'), open)}
      />
    {/if}
    {#if pokerOffered}
      <section class="flex flex-col gap-1.5" data-testid="ticket-poker-section">
        <button
          type="button"
          class="inline-flex min-w-0 items-center gap-1 self-start rounded-[var(--radius-control)] px-1 text-sm font-medium text-fg hover:bg-surface-1 compact:min-h-9 compact:select-none"
          aria-expanded={pokerOpen}
          data-testid="ticket-poker-fold"
          onclick={() => setTicketSectionOpen(section('poker'), !pokerOpen)}
        >
          <Icon icon={pokerOpen ? ChevronDown : ChevronRight} size={13} strokeWidth={2} />
          Planning poker
        </button>
        {#if pokerOpen}
          <TicketPoker
            bind:this={pokerBody}
            {attachment}
            {ctx}
            poker={ticketState?.poker ?? null}
            onSetEstimate={estimateRow ? (value) => { if (estimateRow) setEstimate(estimateRow, value); } : null}
            hours={estimateRow?.unit === 'time'}
          />
        {/if}
      </section>
    {:else if pokerSupportFailure !== null}
      <section class="flex flex-col items-start gap-1 text-sm" data-testid="ticket-poker-section">
        <span class="px-1 font-medium text-fg">Planning poker</span>
        <p class="px-1 text-xs text-error" role="alert" data-testid="ticket-poker-support-error">Could not find out whether this site has planning poker: {pokerSupportFailure}</p>
        <Button size="sm" variant="ghost" testId="ticket-poker-support-retry" onclick={askPokerSupport}>Retry</Button>
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

<!-- svelte-ignore a11y_no_static_element_interactions -->
<div
  class="relative flex h-full min-h-0 min-w-0 flex-col bg-surface-0"
  bind:clientWidth={width}
  data-testid="ticket-screen"
  data-issue={attachment.ref.issue}
  data-narrow={narrow ? '' : undefined}
  ondragover={onDragOver}
  ondragleave={onDragLeave}
  ondrop={onDrop}
  onpaste={onPaste}
>
  {#if dropping}
    <div class="pointer-events-none absolute inset-0 z-[3] flex items-center justify-center border-2 border-dashed border-accent bg-surface-0/80 text-sm text-fg" data-testid="ticket-drop">
      Drop the files to attach them to {ctx.label}
    </div>
  {/if}
  <!-- svelte-ignore a11y_no_static_element_interactions -->
  <header class="flex flex-col gap-1 border-b border-border-subtle px-3 py-2" oncontextmenu={openMenu} data-testid="ticket-header">
    <div class="flex min-w-0 items-center gap-1.5">
      {#if backShown}
        <IconButton label="Back" size="sm" testId="ticket-back" onClick={onBack}>
          {#snippet children()}<Icon icon={ArrowLeft} size={14} strokeWidth={2} />{/snippet}
        </IconButton>
      {/if}
      {#if !linear}
        <button
          bind:this={typeMarkButton}
          type="button"
          class="inline-flex shrink-0 items-center rounded-[var(--radius-control)] p-0.5 hover:bg-surface-1 focus:outline-none focus-visible:ring-2 focus-visible:ring-accent/40 disabled:opacity-60 compact:min-h-9 compact:min-w-9 compact:justify-center"
          aria-label="Type: {card?.type?.name || 'None'}"
          aria-haspopup="menu"
          disabled={!typeEditor || (ticketWrite(ctx.key, fieldTarget('type'))?.pending ?? false)}
          data-testid="ticket-type-mark"
          onclick={() => { typeMarkPicking = true; }}
        >
          <TypeIcon type={card?.type} testId="ticket-type-mark-icon" />
        </button>
        {#if typeEditor?.kind === 'pick'}
          <TicketPicker
            anchor={typeMarkButton}
            open={typeMarkPicking}
            label="Type"
            load={typeEditor.load}
            checked={typeEditor.checked}
            multi={false}
            onPick={typeEditor.onPick}
            onClose={() => { typeMarkPicking = false; }}
          />
        {/if}
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
      {#if !linear && estimateRow}
        <button
          type="button"
          class="inline-flex shrink-0 items-center gap-1 rounded-[var(--radius-control)] border border-border-subtle px-1.5 text-xs text-fg-muted hover:bg-surface-1 hover:text-fg disabled:opacity-60 compact:min-h-9 compact:select-none"
          aria-label="Estimate: {pointsText || 'none'}"
          title="Change the estimate"
          disabled={pointsPending}
          data-testid="ticket-estimate"
          onclick={() => { void change('points'); }}
        >Estimate <span class="tabular-nums text-fg">{pointsText || '–'}</span></button>
      {/if}
      {#if trackerURL}
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
      {#if canAttach}
        <MenuItem label="Attachment…" testId="ticket-menu-attach" onSelect={() => run(() => attachmentsSection?.pick())} />
      {/if}
      <MenuItem label="Link…" testId="ticket-menu-link" onSelect={() => run(() => linksSection?.add())} />
      {#if !linear}
        <MenuItem
          label="Log time…"
          description={timeCtx ? undefined : 'The app does not know your account on this connection'}
          disabled={!timeCtx}
          testId="ticket-menu-log-time"
          onSelect={() => run(() => { logging = true; })}
        />
      {/if}
      <MenuDivider />
      {#if pokerOffered}
        <MenuSectionHeader label="Planning poker" />
        <MenuItem label="Vote…" disabled={pokerPending} testId="ticket-menu-poker-vote" onSelect={() => run(() => openPoker((body) => body.focusVote()))} />
        <MenuItem label="Reveal votes" disabled={pokerPending} testId="ticket-menu-poker-reveal" onSelect={() => run(revealVotes)} />
        <MenuItem label="Clear votes…" disabled={pokerPending} testId="ticket-menu-poker-clear" onSelect={() => run(() => openPoker((body) => body.askClear()))} />
        {#if estimateRow}
          {@const on = estimateRow}
          {#each pokerEstimates as r (r.value)}
            {@const choice = pokerEstimateChoice(r, on.unit === 'time')}
            <MenuItem label="{choice.action} {choice.votes}" disabled={pointsPending} testId="ticket-menu-poker-estimate" onSelect={() => run(() => setEstimate(on, r.value))} />
          {/each}
        {/if}
        <MenuDivider />
      {/if}
    {/if}
    {#if trackerURL || besideFits}
      <MenuSectionHeader label="Open" />
      {#if trackerURL}
        <MenuItem label="In the tracker" testId="ticket-menu-open-tracker" onSelect={() => run(openInTracker)} />
      {/if}
      {#if besideFits}
        <MenuItem label="Over the board" checked={placement === 'over'} onSelect={() => run(() => onPlacement('over'))} />
        <MenuItem label="Beside the board" checked={placement === 'beside'} onSelect={() => run(() => onPlacement('beside'))} />
      {/if}
      <MenuDivider />
    {/if}
    <MenuItem label="Refresh" testId="ticket-menu-refresh" disabled={!ticketState || ticketState.ended} onSelect={() => run(refresh)} />
    <MenuDivider />
    <MenuSectionHeader label="Copy" />
    <MenuItem label="Key" testId="ticket-menu-copy-key" onSelect={() => run(() => copy(ctx.label, 'key'))} />
    {#if trackerURL}
      {@const url = trackerURL}
      <MenuItem label="Link" testId="ticket-menu-copy-link" onSelect={() => run(() => copy(url, 'link'))} />
    {/if}
    <MenuDivider />
    {#if trail > 0}
      <MenuItem label="Back" testId="ticket-menu-back" onSelect={() => run(onBack)} />
    {/if}
    <MenuItem label="Close" testId="ticket-menu-close" onSelect={() => run(onClose)} />
  </ContextMenu>
{/if}

{#if logging && timeCtx}
  <Popover anchor={menuButton} open={true} role="dialog" ariaLabel="Log time" onClose={() => { logging = false; }}>
    <WorklogForm
      ctx={timeCtx}
      ticket={{ issue: attachment.ref.issue, key: card?.key || seed.key, title: card?.title || seed.title }}
      day={localDay(new Date())}
      onDone={() => { logging = false; }}
    />
  </Popover>
{/if}
