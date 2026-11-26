<script module lang="ts">
  import type { FieldEditInput } from '../../../../bindings/agent-overflow/internal/trackerapp/models';
  import type { PickItem } from './TicketPicker.svelte';

  /** A required field as filled in a form. */
  export interface FilledField {
    readonly edit: FieldEditInput;
    readonly picked: readonly PickItem[];
    readonly text: string;
  }

  /** The project and type a form's field values are given for: the
   * project's ID and the type's, empty for a tracker without types. */
  export function valuesOf(project: string, type: string): string {
    return `${project}\n${type}`;
  }

  const NO_FILLED: ReadonlyMap<string, FilledField> = new Map();
  const NO_TYPING: ReadonlyMap<string, string> = new Map();

  /** A ticket the form made for the section's sprint whose move into it
   * did not apply: its ID, its key (empty when the computer could not
   * name it), and what is known of that move, as a sentence (refused, or
   * not known whether it applied). */
  export interface UnplacedTicket {
    readonly issue: string;
    readonly key: string;
    readonly said: string;
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
    // What must hold: the values given for the fields and the text being
    // typed in one are kept for the project and type in effect when they
    // were given (`valuesOf`), each pair its own. The form shows, and Add
    // sends, only those of the project and type it shows; whatever changes
    // them (a pick, the board's projects arriving without the one shown, a
    // read answering again), coming back to a pair shows what was given
    // for it, and nothing given for one replaces another's. A made ticket
    // clears them all (`clearValues`).
    #values = $state.raw<ReadonlyMap<string, { readonly filled: ReadonlyMap<string, FilledField>; readonly typing: ReadonlyMap<string, string> }>>(new Map());
    // What must hold: a ticket made for the sprint and not in it is
    // offered that move until its own move applies, the board shows the
    // ticket in that sprint however it got there, or the person dismisses
    // it; the offer says what is known of the first move. This holds the
    // tickets owed a place, written by Add whether or not the form is on
    // screen when the tracker answers, and dropped by an applied move from
    // the offer or by Dismiss; while one is held, whether it is offered is
    // the sprint's rows' to say (`owed`).
    unplaced = $state.raw<readonly UnplacedTicket[]>([]);

    constructor(project: PickItem | null, type: string) {
      this.project = project;
      this.type = type;
    }

    /** The fields filled for `of`, by field. */
    filled(of: string): ReadonlyMap<string, FilledField> {
      return this.#values.get(of)?.filled ?? NO_FILLED;
    }

    /** The text being typed for `of` in a field not saved yet, by field. */
    typing(of: string): ReadonlyMap<string, string> {
      return this.#values.get(of)?.typing ?? NO_TYPING;
    }

    /** Fills field `id` for `of`, or with null leaves it unfilled. */
    fill(of: string, id: string, field: FilledField | null): void {
      this.#values = withEntry(this.#values, of, { filled: withEntry(this.filled(of), id, field), typing: this.typing(of) });
    }

    /** Holds the text being typed for `of` in field `id`, or with null
     * lets it go. */
    hold(of: string, id: string, text: string | null): void {
      this.#values = withEntry(this.#values, of, { filled: this.filled(of), typing: withEntry(this.typing(of), id, text) });
    }

    clearValues(): void {
      this.#values = new Map();
    }
  }

  function withEntry<T>(m: ReadonlyMap<string, T>, id: string, v: T | null): ReadonlyMap<string, T> {
    const next = new Map(m);
    if (v === null) next.delete(id);
    else next.set(id, v);
    return next;
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
  // says so and offers that move while the sprint's rows do not show it
  // (CreateDraft.unplaced); a refusal stays under it with what was filled.
  import Plus from '@lucide/svelte/icons/plus';
  import type { SvelteMap } from 'svelte/reactivity';
  import Button from '../primitives/Button.svelte';
  import Icon from '../primitives/Icon.svelte';
  import TicketField from './TicketField.svelte';
  import TicketWriteNote from './TicketWriteNote.svelte';
  import { cardFieldName, fieldEditor, heldChoices, mustSupply, pickEditor, type EditorTarget, type HeldList } from './ticketEditors';
  import type { ChoiceView, FieldView, IssueTypeView, ProjectView } from '../../../../bindings/agent-overflow/internal/trackerapp/models';
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

  // The lists the form holds, as their pickers search and show them: a
  // project by its name or key, a type that is not a sub-task by its name.
  const PROJECT_LIST: HeldList<ProjectView> = { searched: [(p) => p.name, (p) => p.key], choice: (p) => ({ id: p.id, name: `${p.name} (${p.key})` }) };
  const TYPE_LIST: HeldList<IssueTypeView> = { searched: [(t) => t.name ?? ''], choice: (t) => ({ id: t.id, name: t.name || t.id }), offered: (t) => t.level >= 0 };
  const ALLOWED_LIST: HeldList<ChoiceView> = { searched: [(c) => c.name ?? ''], choice: (c) => ({ id: c.id, name: c.name ?? '' }) };

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
    if (project && listed.some((p) => p.id === project)) return project;
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

  // The project and type the form's values are for (CreateDraft).
  const of = $derived(valuesOf(projectID, typeID));
  const filled = $derived(draft?.filled(of));

  // A create sends a field only with a value: one picked or typed back to
  // none leaves the form, so the tracker fills it or the form asks again.
  function fill(id: string, field: FilledField | null): void {
    draft?.fill(of, id, field);
  }

  const projectEditor = $derived(pickEditor('project', 'option', {
    picked: projectID ? [{ id: projectID, name: projectName }] : [],
    text: '',
    choices: async (text) => {
      await loadBoardProjects(b);
      return heldChoices(boardProjects(b)?.value ?? [], text, PROJECT_LIST);
    },
    save: (_edit, picked) => {
      if (!draft) return;
      const p = picked[0];
      const listed = p ? boardProjects(b)?.value?.find((it) => it.id === p.id) : undefined;
      draft.project = p ? { id: p.id, name: listed?.name ?? p.name } : null;
      draft.type = '';
      read();
    },
  }));

  const typeEditor = $derived(pickEditor('type', 'option', {
    picked: typeID ? [{ id: typeID, name: typeName }] : [],
    text: '',
    choices: async (text) => heldChoices(types?.value ?? [], text, TYPE_LIST),
    save: (_edit, picked) => {
      if (!draft) return;
      draft.type = picked[0]?.id ?? '';
      readFields();
    },
  }));

  function targetOf(f: FieldView): EditorTarget {
    const got = filled?.get(f.id);
    return {
      picked: got?.picked ?? [],
      text: got?.text ?? '',
      // The values the screen lists ride with the field; only a field it
      // lists none for (users, or priorities the site holds) is read.
      choices: f.allowed
        ? async (text) => heldChoices(f.allowed!, text, ALLOWED_LIST)
        : (text) => createChoices(b, projectID, typeID, f.id, text),
      save: (edit, picked) => { fill(f.id, picked.length || edit.typed?.length ? { edit, picked, text: edit.typed?.[0] ?? '' } : null); },
      // A create sends no value for a field left to the tracker.
      unset: f.defaulted ? TRACKER_DEFAULT : undefined,
      typing: {
        text: draft?.typing(of).get(f.id) ?? null,
        set: (text) => draft?.hold(of, f.id, text),
      },
    };
  }
  // A defaulted field the app cannot set is left to the tracker, unshown.
  const rows = $derived(offered
    .map((f) => ({ f, long: f.kind === 'rich_text' && f.editable, editor: f.kind === 'rich_text' ? null : fieldEditor(f, targetOf(f)) }))
    .filter((r) => mustSupply(r.f) || r.long || r.editor));
  const unsettable = $derived(rows.filter((r) => !r.long && !r.editor).map((r) => r.f.name));
  const missing = $derived(rows.filter((r) => mustSupply(r.f) && !filled?.has(r.f.id)).map((r) => r.f.name));
  const readError = $derived(projects?.error ?? types?.error ?? fields?.error ?? null);
  const ready = $derived(!!projectID && (!typed || !!typeID) && !!fields?.value && unsettable.length === 0);

  function shown(f: FieldView): string {
    const got = filled?.get(f.id);
    return got ? (got.picked.length ? got.picked.map((p) => p.name).join(', ') : got.text) : '';
  }

  async function submit(): Promise<void> {
    const d = draft;
    const text = d?.title.trim() ?? '';
    if (!d || !text || !ready || pending || missing.length > 0) return;
    const sent = { project: projectID, type: typeID };
    const given = d.filled(of);
    setLastCreateChoice(boardRef, typed ? { project: { id: projectID, name: projectName }, type: typeID } : {});
    const outcome = await createTicket(b, sec.list, where, {
      project: sent.project,
      type: sent.type || undefined,
      title: text,
      fields: [...given.values()].map((f) => f.edit),
      sprint: sec.sprint?.id,
    });
    if (outcome?.kind === 'applied') {
      const u = outcome.unplaced;
      if (u?.step === 'sprint_move' && u.issue) {
        // The offer says it from here on: the note would outlast it.
        if (!d.unplaced.some((t) => t.issue === u.issue)) d.unplaced = [...d.unplaced, { issue: u.issue, key: u.key, said: outcome.note ?? '' }];
        dismissTicketWrite(b.key, target);
      }
      d.title = '';
      d.clearValues();
      queueMicrotask(() => input?.focus());
    } else if (outcome?.kind === 'refused' && outcome.refusal.reason === 'missing') {
      // The screen requires what the form did not ask: read it again.
      void reloadCreateFields(b.backend, b.connection, sent.project, sent.type);
    }
  }

  // The made tickets the sprint's rows do not show.
  const owed = $derived((draft?.unplaced ?? []).filter((t) => !sec.rows.some((r) => r.card.id === t.issue)));

  // The made ticket's move into the sprint, the menu's: applied, the
  // ticket is owed nothing, whether or not the board's read after it
  // shows it there yet; refused, not known to have applied, or not sent,
  // it stays offered, its outcome said where a ticket's move says it. A
  // press that sent nothing (the ticket's other move is out) changes
  // nothing.
  async function moveUnplaced(d: CreateDraft, t: UnplacedTicket): Promise<void> {
    if (!sec.sprint) return;
    const outcome = await moveCardToSprint(boardKey, issueWriteContext(boardRef, t.issue, t.key), b.board, sec.sprint.id);
    if (outcome?.kind === 'applied') dismiss(d, t);
  }

  function dismiss(d: CreateDraft, t: UnplacedTicket): void {
    d.unplaced = d.unplaced.filter((u) => u.issue !== t.issue);
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
    {#each rows as { f, long, editor } (`${of}\n${f.id}`)}
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
      {#each owed as t (t.issue)}
        {@const d = draft}
        <div class="flex flex-wrap items-center gap-x-2 gap-y-0.5 text-xs" role="status" data-testid="issues-backlog-create-unplaced" data-issue={t.issue}>
          <span class="text-fg" data-testid="issues-backlog-create-unplaced-said">{t.said}</span>
          <button type="button" class="text-accent hover:underline disabled:opacity-60" data-testid="issues-backlog-create-move" disabled={cardBusy(boardKey, issueWriteContext(boardRef, t.issue, t.key))} onclick={() => { void moveUnplaced(d, t); }}>Move {t.key || 'it'} into {sec.sprint.name}</button>
          <button type="button" class="text-fg-muted hover:underline" data-testid="issues-backlog-create-move-dismiss" aria-label="Leave {t.key || 'the ticket'} out of {sec.sprint.name}" onclick={() => dismiss(d, t)}>Dismiss</button>
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
