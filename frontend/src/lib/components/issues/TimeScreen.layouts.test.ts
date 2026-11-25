// The Time screen derives each layout's rows only while that layout
// shows: By day builds no ticket rows, By ticket builds them.

import { fireEvent, render } from '@testing-library/svelte';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { WorklogView } from '../../../../bindings/agent-overflow/internal/trackerapp/models';
import TimeScreen from './TimeScreen.svelte';
import { setBindingMock } from '../../../test/mocks/bindings-app';

vi.mock('../../stores/trackerTimeModel', async (importOriginal) => {
  const actual = await importOriginal<typeof import('../../stores/trackerTimeModel')>();
  return { ...actual, weekTickets: vi.fn(actual.weekTickets) };
});

const { weekTickets } = await import('../../stores/trackerTimeModel');

const WEEK: WorklogView[] = [
  { id: '501', issue: '10001', key: 'WEB-1', title: 'First story', author: 'acct-1', day: '2026-11-09', spent_seconds: 5400, updated: 1 },
];

beforeEach(() => {
  vi.useFakeTimers({ toFake: ['Date'] });
  vi.setSystemTime(new Date(2026, 10, 12, 10, 0));
  setBindingMock('TrackerWorklogs', async () => ({ worklogs: WEEK }));
  setBindingMock('RefreshTrackerWorklogs', async () => ({ worklogs: WEEK }));
  vi.mocked(weekTickets).mockClear();
});

afterEach(() => {
  vi.useRealTimers();
});

describe('the rows each layout derives', () => {
  it('builds ticket rows only once By ticket shows', async () => {
    const view = render(TimeScreen, {
      backend: 'peer-1', connection: 'c1', connectionKind: 'jira', connectionOn: true, author: 'acct-1',
      computerName: 'Laptop', offline: false, compact: false, shown: true, openIssue: null, onOpenTicket: () => {},
    });
    await view.findAllByTestId('time-entry');
    expect(weekTickets).not.toHaveBeenCalled();
    await fireEvent.click(view.getByRole('radio', { name: 'By ticket' }));
    await view.findAllByTestId('time-ticket-row');
    expect(weekTickets).toHaveBeenCalled();
  });
});
