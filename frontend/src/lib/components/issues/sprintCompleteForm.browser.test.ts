// Real-Chromium suite for completing a sprint (SprintCompleteForm): the
// form says how much is done and how many of the tickets the board shows
// are not in a finished status, both by the tracker's status groups whatever
// the project regroups, always shows where the tracker's
// unfinished ones go, the first future sprint unless another is picked,
// and before the sprint is read whole says it cannot count them yet; a
// destination a refresh took from the list is not sent.

import { afterEach, describe, expect, it } from 'vitest';
import { flushSync, mount, unmount } from 'svelte';
import '../../../app.css';
import SprintCompleteForm from './SprintCompleteForm.svelte';
import type { RowView, SprintView, WriteView } from '../../../../bindings/agent-overflow/internal/trackerapp/models';
import type { BacklogSection } from '../../stores/trackerBacklogModel';
import type { HeldBoard } from '../../stores/trackerBoardModel';
import { ticketWriteSurfaceContext } from './ticketWriteSurface';
import { boardWriteContext } from '../../stores/trackerTicketWrites.svelte';
import { HOME_BACKEND } from '../../transport/backendKey';
import { waitFor } from '../../../test/helpers/browserFrames';
import { setBindingMock } from '../../../test/mocks/bindings-app';
import { rawProps } from '../../../test/helpers/rawProps.svelte';

const mounted: object[] = [];
afterEach(() => {
  for (const app of mounted.splice(0)) unmount(app);
});

const B = boardWriteContext({ backend: HOME_BACKEND, connection: 'c1', board: '1' });
const sprint = (id: string, name: string, state: string) => ({ id, name, state, total: { rows: 0, estimate: 0 } }) as SprintView;
const row = (id: string, group: string) => ({ order: id, estimate: 1, card: { id, key: `WEB-${id}`, title: id, status: { id: group, group }, created: 0, updated: 0 } }) as RowView;
const TOTAL = { rows: 3, estimate: 3, unestimated: 0 };
/** A board whose project puts `regrouped` statuses in other groups. */
const held = (regrouped: Record<string, string> = {}) => ({ regrouped: new Map(Object.entries(regrouped)), leftOut: new Set() }) as unknown as HeldBoard;

function section(extra: Partial<BacklogSection>): BacklogSection {
  return {
    list: 'sprint:8', kind: 'active', section: { kind: 'sprint', sprint: '8' }, sprint: sprint('8', 'Sprint 8', 'active'),
    rows: [row('1', 'done'), row('2', 'todo'), row('3', 'in_progress')], unread: false, more: false, total: TOTAL, ...extra,
  } as unknown as BacklogSection;
}

function open(sec: BacklogSection, into: SprintView[] = [sprint('7', 'Sprint 7', 'active'), sprint('9', 'Sprint 9', 'future')], board = held()): { into: SprintView[] } {
  const anchor = document.createElement('button');
  document.body.appendChild(anchor);
  const props = rawProps({ b: B, held: board, sec, moved: [], unit: 'points', into, anchor, onClose: () => {} });
  mounted.push(mount(SprintCompleteForm, { target: document.body, context: ticketWriteSurfaceContext(() => true), props }));
  flushSync();
  return props;
}

const el = <T extends HTMLElement>(id: string) => document.querySelector<T>(`[data-testid="${id}"]`)!;

describe('completing a sprint', () => {
  it('counts the unfinished tickets and sends them to the first future sprint unless the backlog is picked', async () => {
    const completed = setBindingMock('CompleteTrackerSprint', async () => ({ parts: [{ step: 'sprint', state: 'applied' }] }) as WriteView);
    open(section({}));
    await waitFor(() => document.querySelector('[data-testid="sprint-complete-form"]') !== null, 'the form');
    expect(el('sprint-complete-summary').textContent?.replace(/\s+/g, ' ').trim()).toBe('1 of 3 points done. 2 of the tickets this board shows are not in a finished status.');
    const into = el<HTMLSelectElement>('sprint-complete-into');
    expect(into.value).toBe('9');
    expect([...into.options].map((o) => o.text)).toEqual(['Sprint 7', 'Sprint 9', 'The backlog']);
    into.value = '';
    into.dispatchEvent(new Event('change', { bubbles: true }));
    flushSync();
    el('sprint-complete-submit').click();
    await waitFor(() => completed.mock.calls.length === 1, 'the completion');
    expect(completed.mock.calls[0][1]).toEqual({ board: '1', sprint: '8', move_to: undefined });
  });

  it('shows where the tracker\'s unfinished tickets go though every ticket the board shows is finished', async () => {
    const completed = setBindingMock('CompleteTrackerSprint', async () => ({ parts: [{ step: 'sprint', state: 'applied' }] }) as WriteView);
    open(section({ rows: [row('1', 'done')] }));
    await waitFor(() => document.querySelector('[data-testid="sprint-complete-form"]') !== null, 'the form');
    expect(el('sprint-complete-summary').textContent?.replace(/\s+/g, ' ').trim()).toBe('1 of 1 point done. Every ticket this board shows is in a finished status.');
    expect(el<HTMLSelectElement>('sprint-complete-into').value).toBe('9');
    el('sprint-complete-submit').click();
    await waitFor(() => completed.mock.calls.length === 1, 'the completion');
    expect(completed.mock.calls[0][1]).toEqual({ board: '1', sprint: '8', move_to: '9' });
  });

  it('counts by the tracker\'s status groups, not those the project regroups or leaves out', async () => {
    // The board's total is the project's: In progress left out of it.
    open(section({ total: { rows: 2, estimate: 2, unestimated: 0 } }), undefined, held({ todo: 'done', in_progress: 'left_out' }));
    await waitFor(() => document.querySelector('[data-testid="sprint-complete-form"]') !== null, 'the form');
    expect(el('sprint-complete-summary').textContent?.replace(/\s+/g, ' ').trim()).toBe('1 of 3 points done. 2 of the tickets this board shows are not in a finished status.');
  });

  it('sends no destination a refresh took from the list, and asks for another', async () => {
    const completed = setBindingMock('CompleteTrackerSprint', async () => ({ parts: [{ step: 'sprint', state: 'applied' }] }) as WriteView);
    const props = open(section({}), [sprint('7', 'Sprint 7', 'active'), sprint('9', 'Sprint 9', 'future'), sprint('10', 'Sprint 10', 'future')]);
    await waitFor(() => document.querySelector('[data-testid="sprint-complete-form"]') !== null, 'the form');
    const into = el<HTMLSelectElement>('sprint-complete-into');
    into.value = '10';
    into.dispatchEvent(new Event('change', { bubbles: true }));
    flushSync();
    // Sprint 10 was started, or deleted, elsewhere.
    props.into = [sprint('7', 'Sprint 7', 'active'), sprint('9', 'Sprint 9', 'future')];
    flushSync();
    expect(el('sprint-complete-gone').textContent).toBe('That sprint is no longer open. Pick where the unfinished tickets go.');
    expect(el<HTMLButtonElement>('sprint-complete-submit').disabled).toBe(true);
    el<HTMLFormElement>('sprint-complete-form').requestSubmit();
    flushSync();
    expect(completed).not.toHaveBeenCalled();
    into.value = '9';
    into.dispatchEvent(new Event('change', { bubbles: true }));
    flushSync();
    expect(document.querySelector('[data-testid="sprint-complete-gone"]')).toBeNull();
    el('sprint-complete-submit').click();
    await waitFor(() => completed.mock.calls.length === 1, 'the completion');
    expect(completed.mock.calls[0][1]).toEqual({ board: '1', sprint: '8', move_to: '9' });
  });

  it('asks where the unfinished tickets go before the sprint is read whole, saying it cannot count them yet', async () => {
    open(section({ rows: [row('1', 'done')], more: true }));
    await waitFor(() => document.querySelector('[data-testid="sprint-complete-form"]') !== null, 'the form');
    expect(el('sprint-complete-summary').textContent).toContain('The sprint is still being read, so its unfinished tickets are not counted yet.');
    expect(document.querySelector('[data-testid="sprint-complete-into"]')).not.toBeNull();
  });
});
