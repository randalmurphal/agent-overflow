<script module lang="ts">
  import type { FieldEditInput } from '../../../../bindings/agent-overflow/internal/trackerapp/models';
  import type { PickItem } from './TicketPicker.svelte';

  /** A required field as filled in a form. */
  export interface FilledField {
    readonly edit: FieldEditInput;
    readonly picked: readonly PickItem[];
    readonly text: string;
  }

  /** A ticket the form made that its move into the section's sprint did
   * not place. */
  export interface UnplacedTicket {
    readonly issue: string;
    readonly key: string;
  }

  /**
   * An open New ticket form of one section, held by its Backlog screen so
   * that what was typed outlives the form's foot leaving the virtual
   * window, until Cancel, a made ticket's clear, or the screen going. It
   * keeps the project and type it opened with (the board's last choice,
   * else the board's project), so another form's Add changes nothing in
   * it; that choice only seeds the next form.
   */
  export class CreateDraft {
    title = $state('');
    /** The project it makes the ticket in; null asks for one. Its name is
     * empty while only the board's location names it. */
    project = $state.raw<PickItem | null>(null);
    /** The type picked in it, or the board's last one when it opened;
     * empty takes the project's default. */
    type = $state('');
    filled = $state.raw<ReadonlyMap<string, FilledField>>(new Map());
    /** The text being typed in a field not saved yet, by field. */
    typing = $state.raw<ReadonlyMap<string, string>>(new Map());
    /** The tickets it made that are not in the sprint yet, each offered
     * the move until it is placed or dismissed; written by Add alone,
     * whether or not the form is on screen when the tracker answers. */
    unplaced = $state.raw<readonly UnplacedTicket[]>([]);

    constructor(project: PickItem | null, type: string) {
      this.project = project;
      this.type = type;
    }
  }
</script>

<script lang="ts">
  // A ticket made in place at the foot of a Backlog section
  // (docs/specs/issues.md, "Backlog and sprint planning"): New ticket opens
  // the form, its title takes Enter to add and Escape to cancel. The form
  // asks what the tracker requires the person to supply for a ticket of
  // that project and type (`TrackerCreateFields`, its fields marked
  // required), and offers a field the tracker fills when none is given
  // (`defaulted`), each field with the ticket's own editors
  // (`ticketEditors.ts#fieldEditor`), a field whose screen lists its
  // values picked from that list with no read, and lands the ticket in the
  // section: its sprint, or the backlog. On Jira it shows the project, from
  // the board's projects (`TrackerBoardProjects`, which also names the
  // board's location), the one picked last on the board this session
  // first, else the board's location; once the board's projects are read,
  // one of them (the only one when neither is listed, else none, which
  // asks); the type is the one picked last on
  // the board this session, else the project's first at the standard level,
  // else its first that is not a sub-task. What it reads is read on the
  // form's first open, held as the form reads say
  // (`trackerTicketWrites.svelte.ts`, "A form read"), and nothing is read
  // on the screen's open; a create refused for fields the screen now
  // requires reads them again. A made ticket clears the title and keeps
  // the form open for the next; one made whose move into the sprint failed
  // says so and offers that move; a refusal stays under it with what was
  // filled.
  import Plus from '@lucide/svelte/icons/plus';
  import type { SvelteMap } from 'svelte/reactivity';
  import Button from '../primitives/Button.svelte';
  import Icon from '../primitives/Icon.svelte';
  import TicketField from './TicketField.svelte';
  import TicketWriteNote from './TicketWriteNote.svelte';
  import { cardFieldName, fieldEditor, heldChoices, mustSupply, pickEditor, type EditorTarget } from './ticketEditors';
  import type { FieldView } from '../../../../bindings/agent-overflow/internal/trackerapp/models';
  import type { BacklogSection } from '../../stores/trackerBacklogModel';
  import { lastCreateChoice, setLastCreateChoice } from '../../stores/issuesPane.svelte';
  import type { TrackerBoardRef } from '../../stores/trackerBoard.svelte';
  import {
    boardProjects,
    createChoices,
    createFields,
    createTarget,
    createTicket,
    dismissTicketWrite,
    issueTypes,
    loadBoardProjects,
    loadCreateFields,
    loadIssueTypes,
    reloadCreateFields,
    ticketWrite,
    type BoardWriteContext,
  } from '../../stores/trackerTicketWrites.svelte';
  import { cardBusy, issueWriteContext, moveCardToSprint } from '../../stores/trackerBoardCarry.svelte';
  import { errString } from '../../utils/errors';
  import { isLocalEnter } from '../../utils/localEnter';

  interface Props {
    b: BoardWriteContext;
    /** The board's key (`trackerBoardKey`), which its rows' moves are held under. */
    boardKey: string;
    boardRef: TrackerBoardRef;
    sec: BacklogSection;
    /** The project the board is located in (a Linear board's team); empty
     * when the tracker names none. */
    project: string;
    /** The tracker has issue types and projects to pick (Jira, not Linear). */
    typed: boolean;
    /** The screen's open forms, by board key and section list. */
    drafts: SvelteMap<string, CreateDraft>;
  }

  let { b, boardKey, boardRef, sec, project, typed, drafts }: Props = $props();

  // What a field the tracker fills shows, and the row that leaves it to
  // the tracker.
  const TRACKER_DEFAULT = 'The tracker\'s default';

  const target = $derived(createTarget(sec.list));
  const held = $derived(ticketWrite(b.key, target));
  const pending = $derived(held?.pending ?? false);
  const where = $derived(sec.sprint?.name || 'the backlog');

  // A screen that shows another board keeps that board's forms apart.
  const draftKey = $derived(`${boardKey}\n${sec.list}`);
  const draft = $derived(drafts.get(draftKey) ?? null);
  let input: HTMLInputElement | undefined = $state();

  const projects = $derived(typed ? boardProjects(b) : null);
  // The project it makes the ticket in: once the board's projects are
  // read, one of them (the one picked, else the only one, else none, which
  // asks); while that read is out or failed, the one picked or the board's
  // location.
  const projectID = $derived.by(() => {
    if (!typed) return project;
    const chosen = draft?.project?.id ?? '';
    const listed = projects?.value;
    if (!listed) return chosen;
    if (listed.some((p) => p.id === chosen)) return chosen;
    return listed.length === 1 ? listed[0].id : '';
  });
  const projectName = $derived(projects?.value?.find((p) => p.id === projectID)?.name || (draft?.project?.id === projectID ? draft.project.name : '') || '');
  const types = $derived(typed && projectID ? issueTypes(b.backend, b.connection, projectID) : null);
  const typeID = $derived.by(() => {
    if (!typed) return '';
    const list = types?.value;
    if (!list) return '';
    const want = draft?.type;
    if (want && list.some((t) => t.id === want)) return want;
    return (list.find((t) => t.level === 0) ?? list.find((t) => t.level >= 0))?.id ?? '';
  });
  const typeName = $derived(types?.value?.find((t) => t.id === typeID)?.name ?? '');
  // The fields of the type picked: Linear's need no type.
  const fields = $derived(projectID && (!typed || typeID) ? createFields(b.backend, b.connection, projectID, typeID) : null);
  // The required fields: those the person must supply, then those the
  // tracker would fill.
  const offered = $derived((fields?.value ?? []).filter((f) => f.required).sort((a, c) => Number(!mustSupply(a)) - Number(!mustSupply(c))));

  // Each read on a person's act: the form's open, a project or type
  // picked. Started outside any reactive read of them.
  function read(): void {
    // The board's projects may name another project than the one read
    // now: its types are read once they are in.
    if (typed) void loadBoardProjects(b).then(readTypes);
    readTypes();
  }
  function readTypes(): void {
    if (!projectID) return;
    if (typed) {
      void loadIssueTypes(b.backend, b.connection, projectID).then(readFields);
      return;
    }
    readFields();
  }
  function readFields(): void {
    if (projectID && (!typed || typeID)) void loadCreateFields(b.backend, b.connection, projectID, typeID);
  }

  function start(): void {
    const last = lastCreateChoice(boardRef);
    const opening = last.project ?? (project ? { id: project, name: '' } : null);
    drafts.set(draftKey, new CreateDraft(typed ? opening : null, last.type ?? ''));
    read();
    queueMicrotask(() => input?.focus());
  }

  function cancel(): void {
    drafts.delete(draftKey);
  }

  // A create sends a field only with a value: one picked or typed back to
  // none leaves the form, so the tracker fills it or the form asks again.
  function fill(id: string, field: FilledField | null): void {
    if (!draft) return;
    const next = new Map(draft.filled);
    if (field) next.set(id, field);
    else next.delete(id);
    draft.filled = next;
  }

  const projectEditor = $derived(pickEditor('project', 'option', {
    picked: projectID ? [{ id: projectID, name: projectName }] : [],
    text: '',
    choices: async (text) => {
      await loadBoardProjects(b);
      const t = text.toLowerCase();
      const list = boardProjects(b)?.value ?? [];
      return heldChoices(list.map((p) => ({ id: p.id, name: `${p.name} (${p.key})`, key: p.key, plain: p.name })), (p) => !t || p.plain.toLowerCase().startsWith(t) || p.key.toLowerCase().startsWith(t));
    },
    save: (_edit, picked) => {
      if (!draft) return;
      const p = picked[0];
      const listed = p ? boardProjects(b)?.value?.find((it) => it.id === p.id) : undefined;
      draft.project = p ? { id: p.id, name: listed?.name ?? p.name } : null;
      draft.type = '';
      draft.filled = new Map();
      draft.typing = new Map();
      read();
    },
  }));

  const typeEditor = $derived(pickEditor('type', 'option', {
    picked: typeID ? [{ id: typeID, name: typeName }] : [],
    text: '',
    choices: async (text) => {
      const t = text.toLowerCase();
      return heldChoices((types?.value ?? []).map((it) => ({ ...it, name: it.name || it.id })), (it) => it.level >= 0 && (!t || it.name.toLowerCase().startsWith(t)));
    },
    save: (_edit, picked) => {
      if (!draft) return;
      draft.type = picked[0]?.id ?? '';
      draft.filled = new Map();
      draft.typing = new Map();
      readFields();
    },
  }));

  function targetOf(f: FieldView): EditorTarget {
    const got = draft?.filled.get(f.id);
    return {
      picked: got?.picked ?? [],
      text: got?.text ?? '',
      // The values the screen lists ride with the field; only a field it
      // lists none for (users, or priorities the site holds) is read.
      choices: f.allowed
        ? async (text) => heldChoices(f.allowed!, (c) => (c.name ?? '').toLowerCase().startsWith(text.trim().toLowerCase()))
        : (text) => createChoices(b, projectID, typeID, f.id, text),
      save: (edit, picked) => { fill(f.id, picked.length || edit.typed?.length ? { edit, picked, text: edit.typed?.[0] ?? '' } : null); },
      // A create sends no value for a field left to the tracker.
      unset: f.defaulted ? TRACKER_DEFAULT : undefined,
      typing: {
        text: draft?.typing.get(f.id) ?? null,
        set: (text) => {
          if (!draft) return;
          const next = new Map(draft.typing);
          if (text === null) next.delete(f.id);
          else next.set(f.id, text);
          draft.typing = next;
        },
      },
    };
  }
  // A defaulted field the app cannot set is left to the tracker, unshown.
  const rows = $derived(offered
    .map((f) => ({ f, long: f.kind === 'rich_text' && f.editable, editor: f.kind === 'rich_text' ? null : fieldEditor(f, targetOf(f)) }))
    .filter((r) => mustSupply(r.f) || r.long || r.editor));
  const unsettable = $derived(rows.filter((r) => !r.long && !r.editor).map((r) => r.f.name));
  const missing = $derived(rows.filter((r) => mustSupply(r.f) && !draft?.filled.has(r.f.id)).map((r) => r.f.name));
  const readError = $derived(projects?.error ?? types?.error ?? fields?.error ?? null);
  const ready = $derived(!!projectID && (!typed || !!typeID) && !!fields?.value && unsettable.length === 0);

  function shown(f: FieldView): string {
    const got = draft?.filled.get(f.id);
    return got ? (got.picked.length ? got.picked.map((p) => p.name).join(', ') : got.text) : '';
  }

  async function submit(): Promise<void> {
    const d = draft;
    const text = d?.title.trim() ?? '';
    if (!d || !text || !ready || pending || missing.length > 0) return;
    const sent = { project: projectID, type: typeID };
    setLastCreateChoice(boardRef, typed ? { project: { id: projectID, name: projectName }, type: typeID } : {});
    const outcome = await createTicket(b, sec.list, where, {
      project: sent.project,
      type: sent.type || undefined,
      title: text,
      fields: [...d.filled.values()].map((f) => f.edit),
      sprint: sec.sprint?.id,
    });
    if (outcome?.kind === 'applied') {
      const u = outcome.unplaced;
      if (u?.step === 'sprint_move' && u.issue && !d.unplaced.some((t) => t.issue === u.issue)) d.unplaced = [...d.unplaced, { issue: u.issue, key: u.key }];
      d.title = '';
      d.filled = new Map();
      d.typing = new Map();
      queueMicrotask(() => input?.focus());
    } else if (outcome?.kind === 'refused' && outcome.refusal.reason === 'missing') {
      // The screen requires what the form did not ask: read it again.
      void reloadCreateFields(b.backend, b.connection, sent.project, sent.type);
    }
  }

  // The made ticket's move into the sprint, the menu's: placed, it is no
  // longer offered; refused or unconfirmed, it stays offered, its outcome
  // said where a ticket's move says it; a press that sent nothing (the
  // ticket's other move is out) changes nothing.
  async function moveUnplaced(d: CreateDraft, t: UnplacedTicket): Promise<void> {
    if (!sec.sprint) return;
    const outcome = await moveCardToSprint(boardKey, issueWriteContext(boardRef, t.issue, t.key), b.board, sec.sprint.id);
    if (outcome?.kind === 'applied') forget(d, t);
  }

  // Drops the offer, and the create's note when it is that ticket's.
  function forget(d: CreateDraft, t: UnplacedTicket): void {
    d.unplaced = d.unplaced.filter((u) => u.issue !== t.issue);
    const note = held?.outcome;
    if (note?.kind === 'applied' && note.unplaced?.issue === t.issue) dismissTicketWrite(b.key, target);
  }

  function onKey(event: KeyboardEvent): void {
    if (isLocalEnter(event)) {
      event.preventDefault();
      void submit();
    } else if (event.key === 'Escape') {
      event.preventDefault();
      event.stopPropagation();
      cancel();
    }
  }
</script>

{#if !draft}
  <button
    type="button"
    class="inline-flex items-center gap-1.5 self-start rounded-[var(--radius-control)] px-2 py-1 text-xs text-fg-muted hover:bg-surface-1 hover:text-fg focus:outline-none focus-visible:ring-2 focus-visible:ring-accent/40 compact:min-h-9 compact:select-none"
    data-testid="issues-backlog-new"
    onclick={start}
  ><Icon icon={Plus} size={13} strokeWidth={2} />New ticket</button>
{:else}
  <div class="flex flex-col gap-1 rounded-[var(--radius-field)] border border-border-subtle bg-surface-1 p-2" data-testid="issues-backlog-create" data-list={sec.list}>
    <input
      bind:this={input}
      bind:value={draft.title}
      class="h-8 w-full rounded-[var(--radius-field)] border border-accent bg-surface-0 px-2.5 text-sm text-fg focus:outline-none"
      placeholder="What needs doing? Enter to add, Escape to cancel"
      aria-label="New ticket in {where}"
      disabled={pending}
      data-testid="issues-backlog-create-title"
      onkeydown={onKey}
    />
    {#if typed}
      <TicketField label="Project" field="project" value={projectName || (projectID ? 'Reading…' : '')} editor={projectEditor} ticketKey={b.key} target="create-form" fieldName={cardFieldName} disabled={pending} />
    {/if}
    {#if typed && projectID}
      <TicketField label="Type" field="type" value={typeName} editor={types?.value ? typeEditor : null} ticketKey={b.key} target="create-form" fieldName={cardFieldName} disabled={pending} />
    {/if}
    {#each rows as { f, long, editor } (f.id)}
      {#if long}
        <label class="flex flex-col gap-1 py-1 text-xs text-fg-muted" data-testid="issues-backlog-create-text" data-field={f.id}>
          {mustSupply(f) ? `${f.name} (required)` : f.name}
          <textarea
            rows="3"
            class="w-full resize-y rounded-[var(--radius-field)] border border-border-subtle bg-surface-0 px-2 py-1.5 font-mono text-[0.8125rem] text-fg focus:outline-none focus-visible:ring-2 focus-visible:ring-accent/40"
            value={shown(f)}
            disabled={pending}
            oninput={(event) => { const text = event.currentTarget.value; fill(f.id, text ? { edit: { field: f.id, markdown: { markdown: text, from: '' } }, picked: [], text } : null); }}
          ></textarea>
        </label>
      {:else}
        <TicketField label={mustSupply(f) ? `${f.name} (required)` : f.name} field={f.id} value={shown(f) || (f.defaulted ? TRACKER_DEFAULT : '')} {editor} ticketKey={b.key} target="create-form" fieldName={cardFieldName} disabled={pending} />
      {/if}
    {/each}
    {#if readError}
      <p class="text-xs text-error" role="alert" data-testid="issues-backlog-create-read-error">Could not read what a new ticket takes: {errString(readError)} <button type="button" class="text-accent hover:underline" onclick={read}>Retry</button></p>
    {:else if typed && !projectID}
      <p class="text-xs text-fg-muted" data-testid="issues-backlog-create-pick-project">Pick the new ticket's project.</p>
    {:else if projectID && !fields?.value}
      <p class="text-xs text-fg-muted" data-testid="issues-backlog-create-reading">Reading what a new ticket takes…</p>
    {:else if unsettable.length > 0}
      <p class="text-xs text-fg" role="status" data-testid="issues-backlog-create-unsettable">A {typeName || 'ticket'}{typed && projectName ? ` in ${projectName}` : ''} needs {unsettable.join(', ')}, which the app cannot set. Make it in the tracker.</p>
    {/if}
    <TicketWriteNote ticketKey={b.key} {target} fieldName={cardFieldName} />
    {#if sec.sprint}
      {#each draft.unplaced as t (t.issue)}
        {@const d = draft}
        <div class="flex items-center gap-2 text-xs" data-testid="issues-backlog-create-unplaced" data-issue={t.issue}>
          <button type="button" class="text-accent hover:underline disabled:opacity-60" data-testid="issues-backlog-create-move" disabled={cardBusy(boardKey, issueWriteContext(boardRef, t.issue, t.key))} onclick={() => { void moveUnplaced(d, t); }}>Move {t.key || 'it'} into {sec.sprint.name}</button>
          <button type="button" class="text-fg-muted hover:underline" data-testid="issues-backlog-create-move-dismiss" aria-label="Leave {t.key || 'the ticket'} out of {sec.sprint.name}" onclick={() => forget(d, t)}>Dismiss</button>
        </div>
      {/each}
    {/if}
    <div class="flex items-center gap-1.5">
      <Button size="sm" variant="primary" disabled={pending || !ready || !draft?.title.trim() || missing.length > 0} testId="issues-backlog-create-submit" onclick={() => { void submit(); }}>{pending ? 'Adding…' : 'Add'}</Button>
      <Button size="sm" variant="ghost" disabled={pending} testId="issues-backlog-create-cancel" onclick={cancel}>Cancel</Button>
      {#if missing.length > 0 && ready}
        <span class="text-xs text-fg-muted" data-testid="issues-backlog-create-missing">Needs {missing.join(', ')}</span>
      {/if}
    </div>
  </div>
{/if}
