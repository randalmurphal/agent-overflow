// A sprint's forms, as rendered: what the sprint form refuses before it
// sends (no name, one date of two, an end before its start, clearing a
// sprint's dates), what a start
// offers (today, for as long as the sprint was planned), a change sending
// only what changed and a change of nothing sending nothing. The
// completion's form is in sprintCompleteForm.browser.test.ts, since its
// select needs a real browser.

import { fireEvent, render, waitFor } from '@testing-library/svelte';
import { describe, expect, it } from 'vitest';
import type { SprintView, WriteView } from '../../../../bindings/agent-overflow/internal/trackerapp/models';
import SprintForm from './SprintForm.svelte';
import { ticketWriteSurfaceContext } from './ticketWriteSurface';
import { dayAt, dayValue } from '../../stores/trackerBacklogModel';
import { boardWriteContext } from '../../stores/trackerTicketWrites.svelte';
import { HOME_BACKEND } from '../../transport/backendKey';
import { setBindingMock } from '../../../test/mocks/bindings-app';

const B = boardWriteContext({ backend: HOME_BACKEND, connection: 'c1', board: '1' });
const DAY = 86_400_000;
const applied = { parts: [{ step: 'sprint', state: 'applied' }] } as WriteView;

function sprint(extra: Partial<SprintView> = {}): SprintView {
  return { id: '9', name: 'Sprint 9', state: 'future', total: { rows: 0, estimate: 0 }, ...extra } as SprintView;
}

function form(mode: 'create' | 'edit' | 'start', sp: SprintView | null) {
  let closed = 0;
  const view = render(SprintForm, {
    context: ticketWriteSurfaceContext(() => true),
    props: { b: B, mode, sprint: sp, suggested: 'Sprint 10', anchor: undefined, onClose: () => { closed += 1; } },
  });
  return { ...view, closed: () => closed };
}

const input = (el: HTMLElement, value: string) => fireEvent.input(el, { target: { value } });

describe('the sprint form', () => {
  it('refuses no name, one date of two and an end before its start, and creates with what is filled', async () => {
    const created = setBindingMock('CreateTrackerSprint', async () => applied);
    const view = form('create', null);
    const name = view.getByTestId('sprint-form-name') as HTMLInputElement;
    expect(name.value).toBe('Sprint 10');
    const submit = view.getByTestId('sprint-form-submit') as HTMLButtonElement;
    await input(name, ' ');
    expect(view.getByTestId('sprint-form-problem').textContent).toBe('A sprint needs a name.');
    expect(submit.disabled).toBe(true);
    await input(name, 'Sprint 10');
    await input(view.getByTestId('sprint-form-start'), '2026-12-07');
    expect(view.getByTestId('sprint-form-problem').textContent).toBe('Give both dates, or neither.');
    await input(view.getByTestId('sprint-form-end'), '2026-12-01');
    expect(view.getByTestId('sprint-form-problem').textContent).toBe('The end comes after the start.');
    await input(view.getByTestId('sprint-form-end'), '2026-12-18');
    expect(view.queryByTestId('sprint-form-problem')).toBeNull();
    await fireEvent.click(submit);
    await waitFor(() => expect(view.closed()).toBe(1));
    expect(created.mock.calls).toEqual([['c1', { board: '1', name: 'Sprint 10', goal: undefined, start: dayAt('2026-12-07', false), end: dayAt('2026-12-18', true) }]]);
  });

  it('refuses clearing dates a sprint has, which no change can carry, and sends nothing', async () => {
    const edited = setBindingMock('EditTrackerSprint', async () => applied);
    const start = Date.UTC(2026, 11, 7, 12), end = Date.UTC(2026, 11, 18, 12);
    const view = form('edit', sprint({ start, end }));
    await input(view.getByTestId('sprint-form-start'), '');
    await input(view.getByTestId('sprint-form-end'), '');
    expect(view.getByTestId('sprint-form-problem').textContent).toBe('The app cannot clear a sprint\'s dates: change them, or clear them in the tracker.');
    const submit = view.getByTestId('sprint-form-submit') as HTMLButtonElement;
    expect(submit.disabled).toBe(true);
    await fireEvent.click(submit);
    expect(edited).not.toHaveBeenCalled();
  });

  it('starts a sprint from today for as long as it was planned', async () => {
    const started = setBindingMock('StartTrackerSprint', async () => applied);
    const view = form('start', sprint({ start: dayAt('2026-12-07', false)!, end: dayAt('2026-12-07', false)! + 10 * DAY }));
    const today = dayValue(Date.now());
    expect((view.getByTestId('sprint-form-start') as HTMLInputElement).value).toBe(today);
    expect((view.getByTestId('sprint-form-end') as HTMLInputElement).value).toBe(dayValue(Date.now() + 10 * DAY));
    await fireEvent.click(view.getByTestId('sprint-form-submit'));
    await waitFor(() => expect(started).toHaveBeenCalledTimes(1));
    expect(started.mock.calls[0][1]).toEqual({ board: '1', sprint: '9', start: dayAt(today, false), end: dayAt(dayValue(Date.now() + 10 * DAY), true), name: undefined, goal: undefined });
  });

  it('sends only what a change changed, and nothing for a change of nothing', async () => {
    const edited = setBindingMock('EditTrackerSprint', async () => applied);
    const view = form('edit', sprint({ goal: 'Old goal' }));
    await fireEvent.click(view.getByTestId('sprint-form-submit'));
    expect(view.closed()).toBe(1);
    expect(edited).not.toHaveBeenCalled();
    view.unmount();
    const again = form('edit', sprint({ goal: 'Old goal' }));
    await input(again.getByTestId('sprint-form-name'), 'Sprint 9b');
    await fireEvent.click(again.getByTestId('sprint-form-submit'));
    await waitFor(() => expect(edited).toHaveBeenCalledTimes(1));
    expect(edited.mock.calls[0][1]).toEqual({ board: '1', sprint: '9', name: 'Sprint 9b', goal: undefined, start: undefined, end: undefined });
  });
});
