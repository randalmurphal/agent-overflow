// New ticket at a Backlog section's foot, as rendered: it reads nothing
// until it opens; on Jira it shows the project, from the board's projects
// read once, its location's first; a project's types and a type's fields
// are read once each and the fields the person must supply asked, a field
// whose screen lists its values picked from that list; Add makes the
// ticket in the section's sprint, and the next form on the board starts
// from the project and type picked, while a form already open keeps its
// own; what was given is kept for each project and type and shown and
// sent only for the one shown, the board's projects replacing it
// included, and the location is shown before the only one; what was
// typed outlives the form's foot going; a create refused for fields
// reads them again; a ticket made whose move into the sprint failed
// is not made again; a field the tracker fills is offered, not asked, and
// left to the tracker when the app cannot set it; a required field the app
// cannot set says to make the ticket in the tracker.

import { fireEvent, render, waitFor, within } from '@testing-library/svelte';
import { SvelteMap } from 'svelte/reactivity';
import { describe, expect, it } from 'vitest';
import type { FieldView, IssueTypeView, ProjectView, RowView, WriteView } from '../../../../bindings/agent-overflow/internal/trackerapp/models';
import BacklogCreate, { type CreateDraft } from './BacklogCreate.svelte';
import { ticketWriteSurfaceContext } from './ticketWriteSurface';
import { PICK_LIMIT } from './ticketEditors';
import type { BacklogSection } from '../../stores/trackerBacklogModel';
import { boardWriteContext } from '../../stores/trackerTicketWrites.svelte';
import { setLastCreateChoice } from '../../stores/issuesPane.svelte';
import { getToasts } from '../../stores/toast.svelte';
import { HOME_BACKEND } from '../../transport/backendKey';
import { setBindingMock } from '../../../test/mocks/bindings-app';
import { rawProps } from '../../../test/helpers/rawProps.svelte';

const REF = { backend: HOME_BACKEND, projectId: 'p1', connection: 'c1', board: '1' };
const B = boardWriteContext(REF);
const SPRINT: BacklogSection = {
  list: 'sprint:9', kind: 'future', section: { kind: 'sprint', sprint: '9' },
  sprint: { id: '9', name: 'Sprint 9', state: 'future', total: { rows: 0, estimate: 0 } },
  rows: [], unread: false, more: false, total: { rows: 0, estimate: 0 },
} as unknown as BacklogSection;
const BACKLOG: BacklogSection = { ...SPRINT, list: 'backlog', kind: 'backlog', section: { kind: 'backlog' }, sprint: null } as unknown as BacklogSection;

const PROJECTS = [{ id: '10000', key: 'WEB', name: 'Web' }, { id: '10001', key: 'APP', name: 'App' }] as ProjectView[];
// An epic first, as a Jira project lists its types: the default is the
// first at the standard level.
const TYPES = [{ id: '4', name: 'Epic', level: 1 }, { id: '5', name: 'Sub-task', level: -1 }, { id: '6', name: 'Task', level: 0 }, { id: '7', name: 'Bug', level: 0 }] as IssueTypeView[];
const PRIORITY = { id: 'priority', name: 'Priority', kind: 'option', required: true, editable: true } as FieldView;

function menu(): HTMLElement {
  return document.body.querySelector<HTMLElement>('[data-popover]')!;
}

function field(container: HTMLElement, id: string): HTMLElement {
  return container.querySelector<HTMLElement>(`[data-testid="ticket-field"][data-field="${id}"]`)!;
}

function value(container: HTMLElement, id: string): string | undefined {
  return within(field(container, id)).getByTestId('ticket-field-value').textContent?.trim();
}

async function pick(container: HTMLElement, id: string, name: string): Promise<void> {
  await fireEvent.click(within(field(container, id)).getByTestId('ticket-field-value'));
  const item = await waitFor(() => within(menu()).getAllByTestId('ticket-picker-item').find((el) => el.textContent?.includes(name))!);
  await fireEvent.click(item);
}

function open(project: string, opts: { drafts?: SvelteMap<string, CreateDraft>; sec?: BacklogSection } = {}) {
  const props = rawProps({ b: B, boardKey: 'board-key', boardRef: REF, sec: opts.sec ?? SPRINT, project, typed: true, drafts: opts.drafts ?? new SvelteMap<string, CreateDraft>() });
  return Object.assign(render(BacklogCreate, { context: ticketWriteSurfaceContext(() => true), props }), { props });
}

/** The section as the board shows it holding `ids`, as its read after a
 * move into it does. */
function holding(sec: BacklogSection, ...ids: string[]): BacklogSection {
  return { ...sec, rows: ids.map((id) => ({ order: id, card: { id, key: `WEB-${Number(id) - 10000}` } }) as RowView) };
}

describe('New ticket', () => {
  it('on a board located in no project asks which, from the board\'s projects, reading them, the types and the fields once each, and makes the ticket in the sprint', async () => {
    const projects = setBindingMock('TrackerBoardProjects', async () => PROJECTS);
    const types = setBindingMock('TrackerIssueTypes', async () => TYPES);
    const fields = setBindingMock('TrackerCreateFields', async () => [PRIORITY]);
    setBindingMock('TrackerChoices', async () => ({ choices: [{ id: '2', name: 'High' }], more: false }));
    const created = setBindingMock('CreateTrackerIssue', async () => ({ parts: [{ step: 'create', state: 'applied' }] }) as WriteView);
    const first = open('');
    expect(projects).not.toHaveBeenCalled();
    await fireEvent.click(first.getByTestId('issues-backlog-new'));
    await waitFor(() => expect(first.getByTestId('issues-backlog-create-pick-project')).toBeTruthy());
    expect(projects.mock.calls).toEqual([['c1', '1']]);
    expect(types).not.toHaveBeenCalled();

    await pick(first.container, 'project', 'App');
    // The project's first type at the standard level, not its epic.
    await waitFor(() => expect(value(first.container, 'type')).toBe('Task'));
    expect(types.mock.calls).toEqual([['c1', '10001']]);
    expect(fields.mock.calls).toEqual([['c1', '10001', '6']]);
    await pick(first.container, 'type', 'Bug');
    await waitFor(() => expect(fields.mock.calls).toEqual([['c1', '10001', '6'], ['c1', '10001', '7']]));
    await waitFor(() => expect(first.getByTestId('issues-backlog-create-missing').textContent).toBe('Needs Priority'));
    await pick(first.container, 'priority', 'High');
    const title = first.getByTestId('issues-backlog-create-title') as HTMLInputElement;
    await fireEvent.input(title, { target: { value: 'Made on a board of two projects' } });
    await fireEvent.keyDown(title, { key: 'Enter' });
    await waitFor(() => expect(created).toHaveBeenCalledTimes(1));
    expect(created.mock.calls[0]).toEqual(['c1', {
      board: '1', project: '10001', type: '7', title: 'Made on a board of two projects', sprint: '9',
      fields: [{ field: 'priority', value: { kind: 'option', choices: [{ id: '2' }] } }],
    }]);
    await waitFor(() => expect(title.value).toBe(''));
    first.unmount();

    // The next form on the board starts from App and Bug, reading nothing
    // it holds.
    const next = open('');
    await fireEvent.click(next.getByTestId('issues-backlog-new'));
    await waitFor(() => expect(value(next.container, 'project')).toBe('App'));
    expect(value(next.container, 'type')).toBe('Bug');
    expect(projects).toHaveBeenCalledTimes(1);
    expect(types).toHaveBeenCalledTimes(1);
    expect(fields).toHaveBeenCalledTimes(2);
    next.unmount();
  });

  it('on a board located in one project shows that project, named by the board\'s projects, and makes the ticket in another the board shows', async () => {
    const projects = setBindingMock('TrackerBoardProjects', async () => PROJECTS);
    setBindingMock('TrackerIssueTypes', async () => TYPES);
    setBindingMock('TrackerCreateFields', async () => []);
    const created = setBindingMock('CreateTrackerIssue', async () => ({ parts: [{ step: 'create', state: 'applied' }] }) as WriteView);
    const view = open('10000');
    await fireEvent.click(view.getByTestId('issues-backlog-new'));
    await waitFor(() => expect(value(view.container, 'project')).toBe('Web'));
    expect(projects).toHaveBeenCalledTimes(1);
    await pick(view.container, 'project', 'App');
    await waitFor(() => expect(value(view.container, 'type')).toBe('Task'));
    const title = view.getByTestId('issues-backlog-create-title') as HTMLInputElement;
    await fireEvent.input(title, { target: { value: 'In the other project' } });
    await waitFor(() => expect((view.getByTestId('issues-backlog-create-submit') as HTMLButtonElement).disabled).toBe(false));
    await fireEvent.keyDown(title, { key: 'Enter' });
    await waitFor(() => expect(created).toHaveBeenCalledTimes(1));
    expect(created.mock.calls[0][1]).toMatchObject({ project: '10001', type: '6' });
    expect(projects).toHaveBeenCalledTimes(1);
    view.unmount();
  });

  it('says a failed read of the board\'s projects, with Retry', async () => {
    let fail = true;
    const projects = setBindingMock('TrackerBoardProjects', async () => {
      if (fail) throw new Error('Jira did not answer');
      return PROJECTS;
    });
    setBindingMock('TrackerIssueTypes', async () => TYPES);
    setBindingMock('TrackerCreateFields', async () => []);
    const view = open('10000');
    await fireEvent.click(view.getByTestId('issues-backlog-new'));
    await waitFor(() => expect(view.getByTestId('issues-backlog-create-read-error').textContent).toContain('Jira did not answer'));
    fail = false;
    await fireEvent.click(within(view.getByTestId('issues-backlog-create-read-error')).getByText('Retry'));
    await waitFor(() => expect(value(view.container, 'project')).toBe('Web'));
    expect(projects).toHaveBeenCalledTimes(2);
    view.unmount();
  });

  it('makes the ticket in the board\'s only project when its location is not among the board\'s projects', async () => {
    setBindingMock('TrackerBoardProjects', async () => [PROJECTS[1]]);
    const types = setBindingMock('TrackerIssueTypes', async () => TYPES);
    setBindingMock('TrackerCreateFields', async () => []);
    const created = setBindingMock('CreateTrackerIssue', async () => ({ parts: [{ step: 'create', state: 'applied' }] }) as WriteView);
    const view = open('10000');
    await fireEvent.click(view.getByTestId('issues-backlog-new'));
    await waitFor(() => expect(value(view.container, 'project')).toBe('App'));
    await waitFor(() => expect(types.mock.calls).toContainEqual(['c1', '10001']));
    const title = view.getByTestId('issues-backlog-create-title') as HTMLInputElement;
    await fireEvent.input(title, { target: { value: 'In the board\'s project' } });
    await waitFor(() => expect((view.getByTestId('issues-backlog-create-submit') as HTMLButtonElement).disabled).toBe(false));
    await fireEvent.keyDown(title, { key: 'Enter' });
    await waitFor(() => expect(created).toHaveBeenCalledTimes(1));
    expect(created.mock.calls[0][1]).toMatchObject({ project: '10001', type: '6' });
    view.unmount();
  });

  it('shows and sends none of what was given for the project it showed once the board\'s projects replace it', async () => {
    let answer: () => void = () => {};
    setBindingMock('TrackerBoardProjects', () => new Promise<ProjectView[]>((resolve) => { answer = () => resolve([PROJECTS[1]]); }));
    setBindingMock('TrackerIssueTypes', async () => TYPES);
    const team = { id: 'customfield_3', name: 'Team', kind: 'option', required: true, editable: true, allowed: [{ id: '1', name: 'Red' }] } as FieldView;
    const code = { id: 'customfield_2', name: 'Team code', kind: 'text', required: true, editable: true } as FieldView;
    // Web's screen asks a team and a code; App's asks only the code.
    setBindingMock('TrackerCreateFields', async (_c: string, project: string) => (project === '10000' ? [team, code] : [code]));
    const created = setBindingMock('CreateTrackerIssue', async () => ({ parts: [{ step: 'create', state: 'applied' }] }) as WriteView);
    const view = open('10000');
    await fireEvent.click(view.getByTestId('issues-backlog-new'));
    // While the board's projects are read, the form is Web's: a team picked
    // and a code being typed.
    await waitFor(() => expect(field(view.container, 'customfield_3')).toBeTruthy());
    await pick(view.container, 'customfield_3', 'Red');
    expect(value(view.container, 'customfield_3')).toBe('Red');
    await fireEvent.click(within(field(view.container, 'customfield_2')).getByTestId('ticket-field-value'));
    await fireEvent.input(within(field(view.container, 'customfield_2')).getByTestId('ticket-field-input'), { target: { value: 'WEB-' } });
    // The board shows App alone: the form is App's, with nothing of Web's.
    answer();
    await waitFor(() => expect(value(view.container, 'project')).toBe('App'));
    await waitFor(() => expect(field(view.container, 'customfield_3')).toBeNull());
    const codeField = await waitFor(() => { const el = field(view.container, 'customfield_2'); expect(el).toBeTruthy(); return el; });
    expect(within(codeField).queryByTestId('ticket-field-input')).toBeNull();
    expect(value(view.container, 'customfield_2')).toBe('None');
    await waitFor(() => expect(view.getByTestId('issues-backlog-create-missing').textContent).toBe('Needs Team code'));
    await fireEvent.click(within(codeField).getByTestId('ticket-field-value'));
    const input = within(codeField).getByTestId('ticket-field-input');
    await fireEvent.input(input, { target: { value: 'APP-1' } });
    await fireEvent.keyDown(input, { key: 'Enter' });
    const title = view.getByTestId('issues-backlog-create-title') as HTMLInputElement;
    await fireEvent.input(title, { target: { value: 'In App' } });
    await fireEvent.keyDown(title, { key: 'Enter' });
    await waitFor(() => expect(created).toHaveBeenCalledTimes(1));
    expect(created.mock.calls[0][1]).toMatchObject({ project: '10001', fields: [{ field: 'customfield_2', typed: ['APP-1'] }] });
    expect((created.mock.calls[0][1] as { fields: unknown[] }).fields).toHaveLength(1);
    view.unmount();
  });

  for (const by of ['type', 'project'] as const) {
    for (const edited of [false, true]) {
      it(`keeps what was given for each ${by}: back from another, ${edited ? 'given a value there' : 'given nothing there'}, it shows and Add sends what was given for it`, async () => {
        setBindingMock('TrackerBoardProjects', async () => PROJECTS);
        setBindingMock('TrackerIssueTypes', async () => TYPES);
        setBindingMock('TrackerCreateFields', async () => [{ ...PRIORITY, allowed: [{ id: '2', name: 'High' }, { id: '3', name: 'Low' }] }]);
        const created = setBindingMock('CreateTrackerIssue', async () => ({ parts: [{ step: 'create', state: 'applied' }] }) as WriteView);
        const [there, back] = by === 'type' ? ['Bug', 'Task'] : ['App', 'Web'];
        const view = open('10000');
        await fireEvent.click(view.getByTestId('issues-backlog-new'));
        await waitFor(() => expect(value(view.container, 'type')).toBe('Task'));
        await pick(view.container, 'priority', 'High');
        await pick(view.container, by, there);
        await waitFor(() => expect(value(view.container, by)).toContain(there));
        await waitFor(() => expect(value(view.container, 'priority')).toBe('None'));
        if (edited) await pick(view.container, 'priority', 'Low');
        await pick(view.container, by, back);
        await waitFor(() => expect(value(view.container, by)).toContain(back));
        await waitFor(() => expect(value(view.container, 'type')).toBe('Task'));
        await waitFor(() => expect(value(view.container, 'priority')).toBe('High'));
        // And there again: what was given there, if anything.
        await pick(view.container, by, there);
        await waitFor(() => expect(value(view.container, 'priority')).toBe(edited ? 'Low' : 'None'));
        await pick(view.container, by, back);
        await waitFor(() => expect(value(view.container, 'priority')).toBe('High'));
        const title = view.getByTestId('issues-backlog-create-title') as HTMLInputElement;
        await fireEvent.input(title, { target: { value: 'Back where it began' } });
        await waitFor(() => expect((view.getByTestId('issues-backlog-create-submit') as HTMLButtonElement).disabled).toBe(false));
        await fireEvent.keyDown(title, { key: 'Enter' });
        await waitFor(() => expect(created).toHaveBeenCalledTimes(1));
        expect(created.mock.calls[0][1]).toMatchObject({ project: '10000', type: '6', fields: [{ field: 'priority', value: { kind: 'option', choices: [{ id: '2' }] } }] });
        expect((created.mock.calls[0][1] as { fields: unknown[] }).fields).toHaveLength(1);
        view.unmount();
      });
    }
  }

  it('makes the ticket in the location when the one picked last is not among several the board shows and the location is', async () => {
    setBindingMock('TrackerBoardProjects', async () => PROJECTS);
    setBindingMock('TrackerIssueTypes', async () => TYPES);
    setBindingMock('TrackerCreateFields', async () => []);
    setLastCreateChoice(REF, { project: { id: '10009', name: 'Gone' }, type: '6' });
    const view = open('10000');
    await fireEvent.click(view.getByTestId('issues-backlog-new'));
    await waitFor(() => expect(value(view.container, 'project')).toBe('Web'));
    expect(view.queryByTestId('issues-backlog-create-pick-project')).toBeNull();
    view.unmount();
  });

  it('asks for a project when neither the one picked last nor the location is among several the board shows', async () => {
    setBindingMock('TrackerBoardProjects', async () => [PROJECTS[1], { id: '10002', key: 'OPS', name: 'Ops' }] as ProjectView[]);
    setBindingMock('TrackerIssueTypes', async () => TYPES);
    setBindingMock('TrackerCreateFields', async () => []);
    const created = setBindingMock('CreateTrackerIssue', async () => ({ parts: [{ step: 'create', state: 'applied' }] }) as WriteView);
    // Picked last on this board, and since gone from its filter.
    setLastCreateChoice(REF, { project: { id: '10009', name: 'Gone' }, type: '6' });
    const view = open('10000');
    await fireEvent.click(view.getByTestId('issues-backlog-new'));
    await waitFor(() => expect(view.getByTestId('issues-backlog-create-pick-project')).toBeTruthy());
    const title = view.getByTestId('issues-backlog-create-title') as HTMLInputElement;
    await fireEvent.input(title, { target: { value: 'Somewhere' } });
    await fireEvent.keyDown(title, { key: 'Enter' });
    expect((view.getByTestId('issues-backlog-create-submit') as HTMLButtonElement).disabled).toBe(true);
    expect(created).not.toHaveBeenCalled();
    view.unmount();
  });

  it('makes the ticket in the location while the board\'s projects are being read, and after that read failed', async () => {
    let answer: () => void = () => {};
    let fail = false;
    setBindingMock('TrackerBoardProjects', () => new Promise<ProjectView[]>((resolve, reject) => {
      answer = () => (fail ? reject(new Error('Jira did not answer')) : resolve(PROJECTS));
    }));
    setBindingMock('TrackerIssueTypes', async () => TYPES);
    setBindingMock('TrackerCreateFields', async () => []);
    const created = setBindingMock('CreateTrackerIssue', async () => ({ parts: [{ step: 'create', state: 'applied' }] }) as WriteView);
    const view = open('10000');
    await fireEvent.click(view.getByTestId('issues-backlog-new'));
    const title = view.getByTestId('issues-backlog-create-title') as HTMLInputElement;
    await fireEvent.input(title, { target: { value: 'While reading' } });
    await waitFor(() => expect((view.getByTestId('issues-backlog-create-submit') as HTMLButtonElement).disabled).toBe(false));
    await fireEvent.keyDown(title, { key: 'Enter' });
    await waitFor(() => expect(created).toHaveBeenCalledTimes(1));
    expect(created.mock.calls[0][1]).toMatchObject({ project: '10000' });
    fail = true;
    answer();
    await waitFor(() => expect(view.getByTestId('issues-backlog-create-read-error')).toBeTruthy());
    await waitFor(() => expect(title.value).toBe(''));
    await fireEvent.input(title, { target: { value: 'After it failed' } });
    await fireEvent.keyDown(title, { key: 'Enter' });
    await waitFor(() => expect(created).toHaveBeenCalledTimes(2));
    expect(created.mock.calls[1][1]).toMatchObject({ project: '10000' });
    view.unmount();
  });

  it('picks a field from the values its screen lists, reading nothing more', async () => {
    setBindingMock('TrackerBoardProjects', async () => PROJECTS);
    setBindingMock('TrackerIssueTypes', async () => TYPES);
    const fields = setBindingMock('TrackerCreateFields', async () => [{ ...PRIORITY, allowed: [{ id: '2', name: 'High' }, { id: '3', name: 'Medium' }] }]);
    const choices = setBindingMock('TrackerChoices', async () => ({ choices: [], more: false }));
    const created = setBindingMock('CreateTrackerIssue', async () => ({ parts: [{ step: 'create', state: 'applied' }] }) as WriteView);
    const view = open('10000');
    await fireEvent.click(view.getByTestId('issues-backlog-new'));
    await waitFor(() => expect(view.getByTestId('issues-backlog-create-missing')).toBeTruthy());
    await pick(view.container, 'priority', 'Medium');
    const title = view.getByTestId('issues-backlog-create-title') as HTMLInputElement;
    await fireEvent.input(title, { target: { value: 'Medium one' } });
    await fireEvent.keyDown(title, { key: 'Enter' });
    await waitFor(() => expect(created).toHaveBeenCalledTimes(1));
    expect(created.mock.calls[0][1]).toMatchObject({ fields: [{ field: 'priority', value: { kind: 'option', choices: [{ id: '3' }] } }] });
    expect(choices).not.toHaveBeenCalled();
    expect(fields).toHaveBeenCalledTimes(1);
    view.unmount();
  });

  it('mounts at most the pick limit of a long listed field, says there are more, and finds one past it by its text', async () => {
    setBindingMock('TrackerBoardProjects', async () => PROJECTS);
    setBindingMock('TrackerIssueTypes', async () => TYPES);
    const allowed = Array.from({ length: 300 }, (_, i) => ({ id: String(i), name: `Team ${String(i).padStart(3, '0')}` }));
    setBindingMock('TrackerCreateFields', async () => [{ id: 'customfield_3', name: 'Team', kind: 'option', required: true, editable: true, allowed } as FieldView]);
    const choices = setBindingMock('TrackerChoices', async () => ({ choices: [], more: false }));
    const view = open('10000');
    await fireEvent.click(view.getByTestId('issues-backlog-new'));
    await fireEvent.click(await waitFor(() => within(field(view.container, 'customfield_3')).getByTestId('ticket-field-value')));
    await waitFor(() => expect(within(menu()).getAllByTestId('ticket-picker-item')).toHaveLength(PICK_LIMIT));
    expect(menu().textContent).toContain('The tracker holds more matches; type to narrow them.');
    await fireEvent.input(within(menu()).getByTestId('ticket-picker-find'), { target: { value: 'Team 29' } });
    await waitFor(() => expect(within(menu()).getAllByTestId('ticket-picker-item').map((el) => el.textContent?.trim())).toEqual(
      Array.from({ length: 10 }, (_, i) => `Team 29${i}`)));
    expect(choices).not.toHaveBeenCalled();
    view.unmount();
  });

  // A held list is searched one way at every size: the picker narrows a
  // whole list itself, and asks the form again for one past the bound.
  async function found(container: HTMLElement, id: string, text: string): Promise<string[]> {
    await fireEvent.click(within(field(container, id)).getByTestId('ticket-field-value'));
    await waitFor(() => expect(within(menu()).getAllByTestId('ticket-picker-item').length).toBeGreaterThan(0));
    await fireEvent.input(within(menu()).getByTestId('ticket-picker-find'), { target: { value: text } });
    await waitFor(() => expect(within(menu()).queryByTestId('ticket-picker-searching')).toBeNull());
    const names = within(menu()).queryAllByTestId('ticket-picker-item').map((el) => el.textContent?.trim() ?? '');
    await fireEvent.keyDown(document.activeElement ?? document.body, { key: 'Escape' });
    await waitFor(() => expect(document.body.querySelector('[data-popover]')).toBeNull());
    return names;
  }

  for (const size of [PICK_LIMIT, PICK_LIMIT + 1]) {
    it(`finds a listed field's values, the board's projects and the types by any part of their names, with ${size} of each`, async () => {
      const two = (i: number) => String(i).padStart(2, '0');
      const projects = Array.from({ length: size }, (_, i) => ({ id: String(20_000 + i), key: `P${i}`, name: `Project ${two(i)}` })) as ProjectView[];
      setBindingMock('TrackerBoardProjects', async () => projects);
      // A sub-task first, which is never offered and not counted.
      const types = [{ id: '5', name: 'Sub-task', level: -1 }, ...Array.from({ length: size }, (_, i) => ({ id: String(100 + i), name: `Kind ${two(i)}`, level: 0 }))] as IssueTypeView[];
      setBindingMock('TrackerIssueTypes', async () => types);
      const allowed = Array.from({ length: size }, (_, i) => ({ id: String(i), name: `Team ${two(i)}` }));
      setBindingMock('TrackerCreateFields', async () => [{ id: 'customfield_3', name: 'Team', kind: 'option', required: true, editable: true, allowed } as FieldView]);
      const view = open(projects[0].id);
      await fireEvent.click(view.getByTestId('issues-backlog-new'));
      await waitFor(() => expect(field(view.container, 'customfield_3')).toBeTruthy());
      // Each text matches inside the names, and past the bound (the 51st) when there is one.
      const fives = Array.from({ length: size }, (_, i) => i).filter((i) => two(i).includes('5'));
      expect(await found(view.container, 'customfield_3', '5')).toEqual(fives.map((i) => `Team ${two(i)}`));
      expect(await found(view.container, 'type', 'ind 5')).toEqual(fives.filter((i) => two(i).startsWith('5')).map((i) => `Kind ${two(i)}`));
      expect(await found(view.container, 'type', 'task')).toEqual([]);
      expect(await found(view.container, 'project', 'ject 5')).toEqual(fives.filter((i) => two(i).startsWith('5')).map((i) => `Project ${two(i)} (P${i})`));
      // A project by its key.
      expect(await found(view.container, 'project', 'p5')).toEqual(fives.filter((i) => String(i).startsWith('5')).map((i) => `Project ${two(i)} (P${i})`));
      view.unmount();
    });
  }

  it('reads the fields again when the tracker refuses the create for fields it requires', async () => {
    setBindingMock('TrackerBoardProjects', async () => PROJECTS);
    setBindingMock('TrackerIssueTypes', async () => TYPES);
    const screens = [[], [{ ...PRIORITY, name: 'Team', id: 'customfield_1' }]];
    const fields = setBindingMock('TrackerCreateFields', async () => screens.shift()!);
    setBindingMock('CreateTrackerIssue', async () => ({
      parts: [{ step: 'create', state: 'refused', refusal: { reason: 'missing', message: 'Team is required.', fields: [{ field: 'customfield_1', message: 'required' }] } }],
    }) as WriteView);
    const view = open('10000');
    await fireEvent.click(view.getByTestId('issues-backlog-new'));
    const title = view.getByTestId('issues-backlog-create-title') as HTMLInputElement;
    await fireEvent.input(title, { target: { value: 'Needs a team now' } });
    await waitFor(() => expect((view.getByTestId('issues-backlog-create-submit') as HTMLButtonElement).disabled).toBe(false));
    await fireEvent.keyDown(title, { key: 'Enter' });
    await waitFor(() => expect(view.getByTestId('issues-backlog-create-missing').textContent).toBe('Needs Team'));
    expect(fields).toHaveBeenCalledTimes(2);
    expect(title.value).toBe('Needs a team now');
    view.unmount();
  });

  it('keeps what was typed when its foot leaves the virtual window, and each open form its own type', async () => {
    setBindingMock('TrackerBoardProjects', async () => PROJECTS);
    setBindingMock('TrackerIssueTypes', async () => TYPES);
    setBindingMock('TrackerCreateFields', async () => []);
    const created = setBindingMock('CreateTrackerIssue', async () => ({ parts: [{ step: 'create', state: 'applied' }] }) as WriteView);
    const drafts = new SvelteMap<string, CreateDraft>();
    const sprint = open('10000', { drafts });
    await fireEvent.click(sprint.getByTestId('issues-backlog-new'));
    await waitFor(() => expect(value(sprint.container, 'type')).toBe('Task'));
    await fireEvent.input(sprint.getByTestId('issues-backlog-create-title'), { target: { value: 'Half typed' } });
    sprint.unmount();

    // Another section's form makes a Bug, which the board remembers.
    const backlog = open('10000', { drafts, sec: BACKLOG });
    await fireEvent.click(backlog.getByTestId('issues-backlog-new'));
    await waitFor(() => expect(value(backlog.container, 'type')).toBe('Task'));
    await pick(backlog.container, 'type', 'Bug');
    const title = backlog.getByTestId('issues-backlog-create-title') as HTMLInputElement;
    await fireEvent.input(title, { target: { value: 'A bug' } });
    await waitFor(() => expect((backlog.getByTestId('issues-backlog-create-submit') as HTMLButtonElement).disabled).toBe(false));
    await fireEvent.keyDown(title, { key: 'Enter' });
    await waitFor(() => expect(created).toHaveBeenCalledTimes(1));
    backlog.unmount();

    // The sprint's form, mounted again, holds what was typed and its Task.
    const again = open('10000', { drafts });
    expect((again.getByTestId('issues-backlog-create-title') as HTMLInputElement).value).toBe('Half typed');
    expect(value(again.container, 'type')).toBe('Task');
    await fireEvent.click(again.getByTestId('issues-backlog-create-cancel'));
    expect(drafts.size).toBe(1);
    again.unmount();
  });

  it('keeps the text being typed in a field when its foot leaves the virtual window, and saves it with Enter there', async () => {
    setBindingMock('TrackerBoardProjects', async () => PROJECTS);
    setBindingMock('TrackerIssueTypes', async () => TYPES);
    setBindingMock('TrackerCreateFields', async () => [{ id: 'customfield_2', name: 'Team code', kind: 'text', required: true, editable: true } as FieldView]);
    const created = setBindingMock('CreateTrackerIssue', async () => ({ parts: [{ step: 'create', state: 'applied' }] }) as WriteView);
    const drafts = new SvelteMap<string, CreateDraft>();
    const first = open('10000', { drafts });
    await fireEvent.click(first.getByTestId('issues-backlog-new'));
    await fireEvent.click(await waitFor(() => within(field(first.container, 'customfield_2')).getByTestId('ticket-field-value')));
    await fireEvent.input(within(field(first.container, 'customfield_2')).getByTestId('ticket-field-input'), { target: { value: 'OPS-' } });
    first.unmount();

    const again = open('10000', { drafts });
    const input = await waitFor(() => within(field(again.container, 'customfield_2')).getByTestId('ticket-field-input') as HTMLInputElement);
    expect(input.value).toBe('OPS-');
    await fireEvent.input(input, { target: { value: 'OPS-7' } });
    await fireEvent.keyDown(input, { key: 'Enter' });
    await waitFor(() => expect(value(again.container, 'customfield_2')).toBe('OPS-7'));
    const title = again.getByTestId('issues-backlog-create-title') as HTMLInputElement;
    await fireEvent.input(title, { target: { value: 'With its team' } });
    await fireEvent.keyDown(title, { key: 'Enter' });
    await waitFor(() => expect(created).toHaveBeenCalledTimes(1));
    expect(created.mock.calls[0][1]).toMatchObject({ fields: [{ field: 'customfield_2', typed: ['OPS-7'] }] });
    again.unmount();
  });

  it('once the tracker made the ticket, is done with its title even when its move into the sprint failed, and offers that move', async () => {
    setBindingMock('TrackerBoardProjects', async () => PROJECTS);
    setBindingMock('TrackerIssueTypes', async () => TYPES);
    setBindingMock('TrackerCreateFields', async () => []);
    const created = setBindingMock('CreateTrackerIssue', async () => ({
      parts: [{ step: 'create', state: 'applied', issue: '10012' }, { step: 'sprint_move', state: 'refused', issue: '10012', refusal: { reason: 'tracker', message: 'Sprint 9 is closed.' } }],
      created: '10012', created_key: 'WEB-12',
    }) as WriteView);
    const moved = setBindingMock('MoveTrackerIssuesToSprint', async () => ({ parts: [{ step: 'sprint_move', state: 'applied', issue: '10012' }] }) as WriteView);
    const view = open('10000');
    await fireEvent.click(view.getByTestId('issues-backlog-new'));
    const title = view.getByTestId('issues-backlog-create-title') as HTMLInputElement;
    await waitFor(() => expect((view.getByTestId('issues-backlog-create-submit') as HTMLButtonElement).disabled).toBe(true));
    await fireEvent.input(title, { target: { value: 'Made once' } });
    await waitFor(() => expect((view.getByTestId('issues-backlog-create-submit') as HTMLButtonElement).disabled).toBe(false));
    await fireEvent.keyDown(title, { key: 'Enter' });
    await waitFor(() => expect(view.getByTestId('issues-backlog-create-unplaced-said').textContent).toBe('Created WEB-12, but it was not moved into its sprint: Sprint 9 is closed.'));
    // The offer says it: no note beside it says it again.
    expect(view.queryByTestId('ticket-write-outcome')).toBeNull();
    // The title went with the made ticket: Add sends nothing more.
    await waitFor(() => expect(title.value).toBe(''));
    expect((view.getByTestId('issues-backlog-create-submit') as HTMLButtonElement).disabled).toBe(true);
    await fireEvent.keyDown(title, { key: 'Enter' });
    expect(created).toHaveBeenCalledTimes(1);
    await fireEvent.click(view.getByTestId('issues-backlog-create-move'));
    await waitFor(() => expect(moved).toHaveBeenCalledTimes(1));
    expect(moved.mock.calls[0]).toEqual(['c1', { board: '1', sprint: '9', issues: ['10012'], prev: undefined, next: undefined }]);
    // Applied: the offer ends, before the board shows the ticket there
    // (its read after the move may fail), so nothing sends it twice.
    await waitFor(() => expect(view.queryByTestId('issues-backlog-create-move')).toBeNull());
    expect(moved).toHaveBeenCalledTimes(1);
    view.unmount();
  });

  it('keeps the move of a made ticket with the section\'s form until its move applies or it is dismissed: across its foot leaving, a press that sends nothing, a refused move and one not known to have applied', async () => {
    setBindingMock('TrackerBoardProjects', async () => PROJECTS);
    setBindingMock('TrackerIssueTypes', async () => TYPES);
    setBindingMock('TrackerCreateFields', async () => []);
    let made = 12;
    let createdAnswer: () => void = () => {};
    let gate: Promise<void> | null = new Promise<void>((resolve) => { createdAnswer = resolve; });
    const creates = setBindingMock('CreateTrackerIssue', async () => {
      if (gate) await gate;
      gate = null;
      const id = String(10000 + made);
      const key = `WEB-${made++}`;
      return {
        parts: [{ step: 'create', state: 'applied', issue: id }, { step: 'sprint_move', state: 'refused', issue: id, refusal: { reason: 'tracker', message: 'Sprint 9 is closed.' } }],
        created: id, created_key: key,
      } as WriteView;
    });
    let answer: (v: WriteView) => void = () => {};
    const moved = setBindingMock('MoveTrackerIssuesToSprint', () => new Promise<WriteView>((resolve) => { answer = resolve; }));
    const drafts = new SvelteMap<string, CreateDraft>();
    const first = open('10000', { drafts });
    await fireEvent.click(first.getByTestId('issues-backlog-new'));
    const title = first.getByTestId('issues-backlog-create-title') as HTMLInputElement;
    await fireEvent.input(title, { target: { value: 'Made while away' } });
    await waitFor(() => expect((first.getByTestId('issues-backlog-create-submit') as HTMLButtonElement).disabled).toBe(false));
    // Add, and the foot leaves the virtual window before the tracker answers.
    await fireEvent.keyDown(title, { key: 'Enter' });
    await waitFor(() => expect(creates).toHaveBeenCalledTimes(1));
    first.unmount();
    createdAnswer();
    // Settled with no form on screen: said in a toast.
    await waitFor(() => expect(getToasts().map((t) => t.message)).toContain('Creating a ticket in Sprint 9: Created WEB-12, but it was not moved into its sprint: Sprint 9 is closed.'));

    const view = open('10000', { drafts });
    const offers = () => view.queryAllByTestId('issues-backlog-create-move').map((el) => el.textContent?.trim());
    expect(offers()).toEqual(['Move WEB-12 into Sprint 9']);
    // A second ticket made here is offered its own move.
    const again = view.getByTestId('issues-backlog-create-title') as HTMLInputElement;
    await fireEvent.input(again, { target: { value: 'Made here' } });
    await waitFor(() => expect((view.getByTestId('issues-backlog-create-submit') as HTMLButtonElement).disabled).toBe(false));
    await fireEvent.keyDown(again, { key: 'Enter' });
    await waitFor(() => expect(offers()).toEqual(['Move WEB-12 into Sprint 9', 'Move WEB-13 into Sprint 9']));

    // While WEB-12's move is out, a second press sends nothing and the offer stays.
    const move12 = () => view.getAllByTestId('issues-backlog-create-move')[0] as HTMLButtonElement;
    await fireEvent.click(move12());
    await waitFor(() => expect(moved).toHaveBeenCalledTimes(1));
    await waitFor(() => expect(move12().disabled).toBe(true));
    await fireEvent.click(move12());
    expect(moved).toHaveBeenCalledTimes(1);
    // Refused: still offered.
    answer({ parts: [{ step: 'sprint_move', state: 'refused', issue: '10012', refusal: { reason: 'tracker', message: 'Sprint 9 is closed.' } }] } as WriteView);
    await waitFor(() => expect(move12().disabled).toBe(false));
    expect(offers()).toEqual(['Move WEB-12 into Sprint 9', 'Move WEB-13 into Sprint 9']);
    // Not known whether it applied: still offered, saying so where a move does.
    await fireEvent.click(move12());
    await waitFor(() => expect(moved).toHaveBeenCalledTimes(2));
    answer({ parts: [{ step: 'sprint_move', state: 'unknown', issue: '10012', failure: { kind: 'transient', message: 'Jira did not answer in full.', at: 1 } }] } as WriteView);
    await waitFor(() => expect(move12().disabled).toBe(false));
    expect(offers()).toEqual(['Move WEB-12 into Sprint 9', 'Move WEB-13 into Sprint 9']);
    // Applied: no longer offered, though the board does not show it there yet.
    await fireEvent.click(move12());
    await waitFor(() => expect(moved).toHaveBeenCalledTimes(3));
    answer({ parts: [{ step: 'sprint_move', state: 'applied', issue: '10012' }] } as WriteView);
    await waitFor(() => expect(offers()).toEqual(['Move WEB-13 into Sprint 9']));
    // Dismissed: no longer offered, and nothing sent.
    await fireEvent.click(view.getByTestId('issues-backlog-create-move-dismiss'));
    expect(view.queryByTestId('issues-backlog-create-move')).toBeNull();
    expect(moved).toHaveBeenCalledTimes(3);
    view.unmount();
  });

  it('says it may not be in its sprint when the move\'s answer was lost, also after its foot comes back and another ticket is made, does not make it again, and stops offering the move once the board shows it there', async () => {
    setBindingMock('TrackerBoardProjects', async () => PROJECTS);
    setBindingMock('TrackerIssueTypes', async () => TYPES);
    setBindingMock('TrackerCreateFields', async () => []);
    let makes = 0;
    const created = setBindingMock('CreateTrackerIssue', async () => (makes++ === 0 ? {
      parts: [{ step: 'create', state: 'applied', issue: '10012' }, { step: 'sprint_move', state: 'unknown', issue: '10012', failure: { kind: 'transient', message: 'Jira did not answer in full.', at: 1 } }],
      created: '10012',
    } : { parts: [{ step: 'create', state: 'applied', issue: '10013' }, { step: 'sprint_move', state: 'applied', issue: '10013' }], created: '10013', created_key: 'WEB-13' }) as WriteView);
    const drafts = new SvelteMap<string, CreateDraft>();
    const view = open('10000', { drafts });
    await fireEvent.click(view.getByTestId('issues-backlog-new'));
    const title = view.getByTestId('issues-backlog-create-title') as HTMLInputElement;
    await fireEvent.input(title, { target: { value: 'Made once' } });
    await waitFor(() => expect((view.getByTestId('issues-backlog-create-submit') as HTMLButtonElement).disabled).toBe(false));
    await fireEvent.click(view.getByTestId('issues-backlog-create-submit'));
    const said = 'Created the ticket; it may not be in its sprint: Jira did not answer in full.';
    await waitFor(() => expect(view.getByTestId('issues-backlog-create-unplaced-said').textContent).toBe(said));
    await waitFor(() => expect(title.value).toBe(''));
    expect(view.getByTestId('issues-backlog-create-move').textContent).toBe('Move it into Sprint 9');
    expect(created).toHaveBeenCalledTimes(1);
    view.unmount();
    // The foot back, and another ticket made: the offer still says what is
    // known of the first move.
    const again = open('10000', { drafts });
    const next = again.getByTestId('issues-backlog-create-title') as HTMLInputElement;
    await fireEvent.input(next, { target: { value: 'Made next' } });
    await waitFor(() => expect((again.getByTestId('issues-backlog-create-submit') as HTMLButtonElement).disabled).toBe(false));
    await fireEvent.keyDown(next, { key: 'Enter' });
    await waitFor(() => expect(created).toHaveBeenCalledTimes(2));
    await waitFor(() => expect(next.value).toBe(''));
    expect(again.getByTestId('issues-backlog-create-unplaced-said').textContent).toBe(said);
    expect(again.getByTestId('issues-backlog-create-move').textContent).toBe('Move it into Sprint 9');
    // Placed another way (the row's menu, another screen): the board shows
    // it in the sprint, and the offer is gone with no move from here.
    again.props.sec = holding(SPRINT, '10012');
    await waitFor(() => expect(again.queryByTestId('issues-backlog-create-unplaced')).toBeNull());
    again.unmount();
  });

  it('offers a field the tracker fills when none is given, sends it only when picked, and leaves one it cannot set to the tracker', async () => {
    setBindingMock('TrackerBoardProjects', async () => PROJECTS);
    setBindingMock('TrackerIssueTypes', async () => TYPES);
    setBindingMock('TrackerCreateFields', async () => [
      { ...PRIORITY, required: true, defaulted: true, allowed: [{ id: '2', name: 'High' }] },
      { id: 'customfield_1', name: 'Team', kind: 'cascading', required: true, defaulted: true, editable: true } as FieldView,
    ]);
    const created = setBindingMock('CreateTrackerIssue', async () => ({ parts: [{ step: 'create', state: 'applied' }] }) as WriteView);
    const view = open('10000');
    await fireEvent.click(view.getByTestId('issues-backlog-new'));
    await waitFor(() => expect(value(view.container, 'priority')).toBe('The tracker\'s default'));
    expect(field(view.container, 'priority')).not.toHaveTextContent('(required)');
    expect(field(view.container, 'customfield_1')).toBeNull();
    expect(view.queryByTestId('issues-backlog-create-unsettable')).toBeNull();
    expect(view.queryByTestId('issues-backlog-create-missing')).toBeNull();
    const title = view.getByTestId('issues-backlog-create-title') as HTMLInputElement;
    await fireEvent.input(title, { target: { value: 'Default one' } });
    await fireEvent.keyDown(title, { key: 'Enter' });
    await waitFor(() => expect(created).toHaveBeenCalledTimes(1));
    expect(created.mock.calls[0][1]).toMatchObject({ fields: [] });
    await waitFor(() => expect(title.value).toBe(''));
    await pick(view.container, 'priority', 'High');
    await fireEvent.input(title, { target: { value: 'High one' } });
    await fireEvent.keyDown(title, { key: 'Enter' });
    await waitFor(() => expect(created).toHaveBeenCalledTimes(2));
    expect(created.mock.calls[1][1]).toMatchObject({ fields: [{ field: 'priority', value: { kind: 'option', choices: [{ id: '2' }] } }] });
    // Picked and then put back to none: the tracker's default again, and
    // nothing sent for it, since Jira refuses a clear of a field it requires.
    await waitFor(() => expect(title.value).toBe(''));
    await pick(view.container, 'priority', 'High');
    await fireEvent.click(within(field(view.container, 'priority')).getByTestId('ticket-field-value'));
    const unset = await waitFor(() => within(menu()).getByTestId('ticket-picker-clear'));
    expect(unset).toHaveTextContent('The tracker\'s default');
    await fireEvent.click(unset);
    await waitFor(() => expect(value(view.container, 'priority')).toBe('The tracker\'s default'));
    await fireEvent.input(title, { target: { value: 'Default again' } });
    await fireEvent.keyDown(title, { key: 'Enter' });
    await waitFor(() => expect(created).toHaveBeenCalledTimes(3));
    expect(created.mock.calls[2][1]).toMatchObject({ fields: [] });
    view.unmount();
  });

  it('says to make the ticket in the tracker when it requires a field the app cannot set', async () => {
    setBindingMock('TrackerBoardProjects', async () => PROJECTS);
    setBindingMock('TrackerIssueTypes', async () => TYPES);
    setBindingMock('TrackerCreateFields', async () => [{ id: 'customfield_1', name: 'Team', kind: 'cascading', required: true, editable: true } as FieldView]);
    const view = open('10000');
    await fireEvent.click(view.getByTestId('issues-backlog-new'));
    await waitFor(() => expect(view.getByTestId('issues-backlog-create-unsettable').textContent).toBe('A Task in Web needs Team, which the app cannot set. Make it in the tracker.'));
    expect((view.getByTestId('issues-backlog-create-submit') as HTMLButtonElement).disabled).toBe(true);
    view.unmount();
  });
});
