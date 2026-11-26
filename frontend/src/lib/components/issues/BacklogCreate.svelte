<script lang="ts">
  // A ticket made in place at the foot of a Backlog section
  // (docs/specs/issues.md, "Backlog and sprint planning"): New ticket opens
  // the form, its title takes Enter to add and Escape to cancel. The form
  // asks what the tracker requires of a ticket of that project and type
  // (`TrackerCreateFields`), each field with the ticket's own editors
  // (`ticketEditors.ts#fieldEditor`), and lands the ticket in the section:
  // its sprint, or the backlog. The type is the one picked last on the
  // board this session, else the project's first that is not a sub-task;
  // a board of several projects asks which, the one picked last on the
  // board this session first, from the connection's projects. What it
  // reads is read on the form's first open, held as the form reads say
  // (`trackerTicketWrites.svelte.ts`, "A form read"), and nothing is read
  // on the screen's open. A made ticket clears the title and keeps the
  // form open for the next; a refusal stays under it with what was filled.
  import Plus from '@lucide/svelte/icons/plus';
  import Button from '../primitives/Button.svelte';
  import Icon from '../primitives/Icon.svelte';
  import TicketField from './TicketField.svelte';
  import TicketWriteNote from './TicketWriteNote.svelte';
  import { fieldEditor, pickEditor, type EditorTarget } from './ticketEditors';
  import type { PickItem } from './TicketPicker.svelte';
  import type { FieldEditInput, FieldView } from '../../../../bindings/agent-overflow/internal/trackerapp/models';
  import type { BacklogSection } from '../../stores/trackerBacklogModel';
  import { lastCreateChoice, setLastCreateChoice } from '../../stores/issuesPane.svelte';
  import type { TrackerBoardRef } from '../../stores/trackerBoard.svelte';
  import {
    createChoices,
    createFields,
    createTarget,
    createTicket,
    issueTypes,
    loadCreateFields,
    loadIssueTypes,
    loadTrackerProjects,
    ticketWrite,
    trackerProjects,
    type BoardWriteContext,
  } from '../../stores/trackerTicketWrites.svelte';
  import { errString } from '../../utils/errors';
  import { isLocalEnter } from '../../utils/localEnter';
  import { cardFieldName } from './ticketEditors';

  interface Props {
    b: BoardWriteContext;
    boardRef: TrackerBoardRef;
    sec: BacklogSection;
    /** The project the board shows; empty for a board of several. */
    project: string;
    /** The tracker has issue types (not Linear). */
    typed: boolean;
  }

  let { b, boardRef, sec, project, typed }: Props = $props();

  const target = $derived(createTarget(sec.list));
  const pending = $derived(ticketWrite(b.key, target)?.pending ?? false);
  const where = $derived(sec.sprint?.name || 'the backlog');

  let open = $state(false);
  let title = $state('');
  // The project and type picked in this form; the board's choice before.
  let pickedProject = $state.raw<PickItem | null>(null);
  let pickedType = $state.raw<PickItem | null>(null);
  // The required fields filled, by field ID.
  let filled = $state.raw<ReadonlyMap<string, { edit: FieldEditInput; picked: readonly PickItem[]; text: string }>>(new Map());
  let input: HTMLInputElement | undefined = $state();

  const projects = $derived(project ? null : trackerProjects(b.backend, b.connection));
  const projectID = $derived(project || pickedProject?.id || lastCreateChoice(boardRef).project || '');
  const projectName = $derived(pickedProject?.name ?? projects?.value?.find((p) => p.id === projectID)?.name ?? projectID);
  const types = $derived(typed && projectID ? issueTypes(b.backend, b.connection, projectID) : null);
  const typeID = $derived.by(() => {
    if (!typed) return '';
    const list = types?.value;
    if (!list) return '';
    const want = pickedType?.id ?? lastCreateChoice(boardRef).type;
    if (want && list.some((t) => t.id === want)) return want;
    return list.find((t) => t.level >= 0)?.id ?? '';
  });
  const typeName = $derived(types?.value?.find((t) => t.id === typeID)?.name ?? '');
  // The fields of the type picked: Linear's need no type.
  const fields = $derived(projectID && (!typed || typeID) ? createFields(b.backend, b.connection, projectID, typeID) : null);
  const required = $derived((fields?.value ?? []).filter((f) => f.required));

  // Each read on a person's act: the form's open, a project or type
  // picked. Started outside any reactive read of them.
  function read(): void {
    if (!project) void loadTrackerProjects(b.backend, b.connection);
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
    open = true;
    read();
    queueMicrotask(() => input?.focus());
  }

  function cancel(): void {
    open = false;
    title = '';
    filled = new Map();
  }

  const projectEditor = $derived(pickEditor('project', 'option', {
    picked: projectID ? [{ id: projectID, name: projectName }] : [],
    text: '',
    choices: async (text) => {
      const list = projects?.value ?? [];
      const t = text.toLowerCase();
      return { choices: list.filter((p) => !t || p.name.toLowerCase().startsWith(t) || p.key.toLowerCase().startsWith(t)).map((p) => ({ id: p.id, name: `${p.name} (${p.key})` })), more: false };
    },
    save: (_edit, picked) => {
      pickedProject = picked[0] ?? null;
      pickedType = null;
      filled = new Map();
      read();
    },
  }));

  const typeEditor = $derived(pickEditor('type', 'option', {
    picked: typeID ? [{ id: typeID, name: typeName }] : [],
    text: '',
    choices: async (text) => {
      const t = text.toLowerCase();
      return { choices: (types?.value ?? []).filter((it) => it.level >= 0 && (!t || (it.name ?? '').toLowerCase().startsWith(t))).map((it) => ({ id: it.id, name: it.name || it.id })), more: false };
    },
    save: (_edit, picked) => {
      pickedType = picked[0] ?? null;
      filled = new Map();
      readFields();
    },
  }));

  function targetOf(f: FieldView): EditorTarget {
    const got = filled.get(f.id);
    return {
      picked: got?.picked ?? [],
      text: got?.text ?? '',
      choices: (text) => createChoices(b, projectID, typeID, f.id, text),
      save: (edit, picked) => { filled = new Map(filled).set(f.id, { edit, picked, text: edit.typed?.[0] ?? '' }); },
    };
  }
  const rows = $derived(required.map((f) => ({ f, long: f.kind === 'rich_text' && f.editable, editor: f.kind === 'rich_text' ? null : fieldEditor(f, targetOf(f)) })));
  const unsettable = $derived(rows.filter((r) => !r.long && !r.editor).map((r) => r.f.name));
  const missing = $derived(rows.filter((r) => !filled.has(r.f.id)).map((r) => r.f.name));
  const readError = $derived(projects?.error ?? types?.error ?? fields?.error ?? null);
  const ready = $derived(!!projectID && (!typed || !!typeID) && !!fields?.value && unsettable.length === 0);

  function shown(f: FieldView): string {
    const got = filled.get(f.id);
    return got ? (got.picked.length ? got.picked.map((p) => p.name).join(', ') : got.text) : '';
  }

  async function submit(): Promise<void> {
    const text = title.trim();
    if (!text || !ready || pending || missing.length > 0) return;
    setLastCreateChoice(boardRef, { ...(project ? {} : { project: projectID }), ...(typed ? { type: typeID } : {}) });
    const outcome = await createTicket(b, sec.list, where, {
      project: projectID,
      type: typeID || undefined,
      title: text,
      fields: [...filled.values()].map((f) => f.edit),
      sprint: sec.sprint?.id,
    });
    if (outcome?.kind === 'applied') {
      title = '';
      filled = new Map();
      queueMicrotask(() => input?.focus());
    }
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

{#if !open}
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
      bind:value={title}
      class="h-8 w-full rounded-[var(--radius-field)] border border-accent bg-surface-0 px-2.5 text-sm text-fg focus:outline-none"
      placeholder="What needs doing? Enter to add, Escape to cancel"
      aria-label="New ticket in {where}"
      disabled={pending}
      data-testid="issues-backlog-create-title"
      onkeydown={onKey}
    />
    {#if !project}
      <TicketField label="Project" field="project" value={projectName} editor={projects?.value ? projectEditor : null} ticketKey={b.key} target="create-form" fieldName={cardFieldName} disabled={pending} />
    {/if}
    {#if typed && projectID}
      <TicketField label="Type" field="type" value={typeName} editor={types?.value ? typeEditor : null} ticketKey={b.key} target="create-form" fieldName={cardFieldName} disabled={pending} />
    {/if}
    {#each rows as { f, long, editor } (f.id)}
      {#if long}
        <label class="flex flex-col gap-1 py-1 text-xs text-fg-muted" data-testid="issues-backlog-create-text" data-field={f.id}>
          {f.name} (required)
          <textarea
            rows="3"
            class="w-full resize-y rounded-[var(--radius-field)] border border-border-subtle bg-surface-0 px-2 py-1.5 font-mono text-[0.8125rem] text-fg focus:outline-none focus-visible:ring-2 focus-visible:ring-accent/40"
            value={shown(f)}
            disabled={pending}
            oninput={(event) => { filled = new Map(filled).set(f.id, { edit: { field: f.id, markdown: { markdown: event.currentTarget.value, from: '' } }, picked: [], text: event.currentTarget.value }); }}
          ></textarea>
        </label>
      {:else}
        <TicketField label="{f.name} (required)" field={f.id} value={shown(f)} {editor} ticketKey={b.key} target="create-form" fieldName={cardFieldName} disabled={pending} />
      {/if}
    {/each}
    {#if readError}
      <p class="text-xs text-error" role="alert" data-testid="issues-backlog-create-read-error">Could not read what a new ticket takes: {errString(readError)} <button type="button" class="text-accent hover:underline" onclick={read}>Retry</button></p>
    {:else if !project && projects?.value && !projectID}
      <p class="text-xs text-fg-muted" data-testid="issues-backlog-create-pick-project">This board shows several projects: pick the new ticket's project.</p>
    {:else if projectID && !fields?.value}
      <p class="text-xs text-fg-muted" data-testid="issues-backlog-create-reading">Reading what a new ticket takes…</p>
    {:else if unsettable.length > 0}
      <p class="text-xs text-fg" role="status" data-testid="issues-backlog-create-unsettable">A {typeName || 'ticket'}{project ? '' : ` in ${projectName}`} needs {unsettable.join(', ')}, which the app cannot set. Make it in the tracker.</p>
    {/if}
    <TicketWriteNote ticketKey={b.key} {target} fieldName={cardFieldName} />
    <div class="flex items-center gap-1.5">
      <Button size="sm" variant="primary" disabled={pending || !ready || !title.trim() || missing.length > 0} testId="issues-backlog-create-submit" onclick={() => { void submit(); }}>{pending ? 'Adding…' : 'Add'}</Button>
      <Button size="sm" variant="ghost" disabled={pending} testId="issues-backlog-create-cancel" onclick={cancel}>Cancel</Button>
      {#if missing.length > 0 && ready}
        <span class="text-xs text-fg-muted" data-testid="issues-backlog-create-missing">Needs {missing.join(', ')}</span>
      {/if}
    </div>
  </div>
{/if}
