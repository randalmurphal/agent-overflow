// A Time screen's methods and a sprint edit over the wire, from a client
// to the fake Jira's own worklogs and sprints and back: before a refresh
// the mirror read answers every day unread, time logged in Jira is read
// by a refresh and then by the mirror read with no tracker request, a
// worklog added, edited and deleted through the app each show in the next
// mirror read, a refresh of a person Jira does not have answers Jira's
// refusal as its failure, a refresh of more than 31 days is refused with
// a sentence and no tracker request, and a sprint renamed through the app
// is renamed in Jira.
//
// The refusals of a tracker with no time tracking and of a mirror read
// past 366 days are TestTrackerTimeReachesItsClient (internal/app); each
// method's scope, route and budget are
// TestTrackerTicketMethodsCarryTheirScopeAndRoute.

import { test, expect } from './fixtures.js';
import { connectFakeJiraBoard, JIRA_ACCOUNT, JIRA_TOKEN, SPRINT_ID, BOARD_ID, type TrackerConnection } from './tracker-helpers.js';

interface Worklog { id: string; issue: string; author: string; day: string; spent_seconds: number; description?: string; updated?: number }
interface WorklogDays { worklogs: Worklog[]; more?: boolean; unread?: string[]; failure?: { kind: string; message: string } }
interface WriteView { parts: { step: string; state: string }[] }

const auth = { authorization: `Basic ${Buffer.from(`${JIRA_ACCOUNT}:${JIRA_TOKEN}`).toString('base64')}` };

test('a Time screen reads, adds, edits and deletes worklogs, and a sprint edit renames the sprint', async ({ harness }) => {
  const board = await connectFakeJiraBoard(harness, 'tracker-time-wire');
  // The author a Time screen reads: the account the connection verified as.
  const connections = await harness.rpc<Array<TrackerConnection & { verifiedAccountId: string }>>('TrackerConnections');
  const author = connections.find((c) => c.id === board.connectionId)!.verifiedAccountId;
  expect(author).toBe('acc-user1');
  const now = new Date();
  const day = (back: number) => new Date(now.getTime() - back * 24 * 3600_000).toISOString().slice(0, 10);
  const today = day(0);
  const weekAgo = day(6);

  // Time logged in Jira itself, two days ago.
  const logged = await fetch(`${board.jiraURL}/rest/api/3/issue/WEB-2/worklog`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json', ...auth },
    body: JSON.stringify({ started: `${day(2)}T09:00:00.000+0000`, timeSpentSeconds: 7200 }),
  });
  expect(logged.status, await logged.clone().text()).toBe(201);

  const requests = async () => (await harness.rpc<{ requests: unknown[] }>('HarnessTrackerRequests', 0)).requests.length;
  const mirrored = () => harness.rpc<WorklogDays>('TrackerWorklogs', board.connectionId, author, weekAgo, today);
  const unread = await mirrored();
  expect(unread.worklogs).toEqual([]);
  expect(unread.unread, 'before a refresh every day is unread').toEqual([6, 5, 4, 3, 2, 1, 0].map(day));

  const beforeRefresh = await requests();
  const refreshed = await harness.rpc<WorklogDays>('RefreshTrackerWorklogs', board.connectionId, author, weekAgo, today);
  expect(refreshed.failure).toBeUndefined();
  expect(refreshed.unread).toBeUndefined();
  expect(refreshed.worklogs).toEqual([expect.objectContaining({ issue: '10102', author, day: day(2), spent_seconds: 7200 })]);
  const inJira = refreshed.worklogs[0];

  // The refresh read Jira; the mirror read sends the tracker nothing.
  const afterRefresh = await requests();
  expect(afterRefresh, 'the refresh read Jira').toBeGreaterThan(beforeRefresh);
  expect((await mirrored()).worklogs).toEqual([inJira]);
  expect(await requests(), 'the mirror read sent the tracker a request').toBe(afterRefresh);

  const add = await harness.rpc<WriteView>('AddTrackerWorklog', board.connectionId, { issue: '10101', day: today, spent_seconds: 1800 });
  expect(add.parts.map((p) => [p.step, p.state])).toEqual([['worklog', 'applied']]);
  const added = (await mirrored()).worklogs.find((w) => w.issue === '10101');
  expect(added, 'the mirror read shows the worklog added').toMatchObject({ day: today, spent_seconds: 1800 });

  const edit = await harness.rpc<WriteView>('EditTrackerWorklog', board.connectionId, { from: added, spent_seconds: 3600 });
  expect(edit.parts.map((p) => [p.step, p.state])).toEqual([['worklog', 'applied']]);
  const del = await harness.rpc<WriteView>('DeleteTrackerWorklog', board.connectionId, { from: inJira });
  expect(del.parts.map((p) => [p.step, p.state])).toEqual([['worklog', 'applied']]);
  expect((await mirrored()).worklogs).toEqual([expect.objectContaining({ id: added!.id, issue: '10101', spent_seconds: 3600 })]);

  // Jira's 404 for the person is the refresh's failure, not the
  // connection's problem.
  const nobody = await harness.rpc<WorklogDays>('RefreshTrackerWorklogs', board.connectionId, 'acc-nobody', weekAgo, today);
  expect(nobody.failure).toEqual(expect.objectContaining({ kind: 'tracker',
    message: "Jira has no person with account ID acc-nobody, or the token's user may not see them; choose another person" }));
  const after404 = await harness.rpc<Array<TrackerConnection & { problem?: unknown }>>('TrackerConnections');
  expect(after404.find((c) => c.id === board.connectionId)!.problem).toBeFalsy();

  const beforeLong = await requests();
  await expect(harness.rpc('RefreshTrackerWorklogs', board.connectionId, author, day(31), today))
    .rejects.toThrow(/^tracker_request_invalid: nothing was sent: a worklog refresh reads at most 31 days/);
  expect(await requests(), 'the refused refresh sent the tracker a request').toBe(beforeLong);

  const sprint = await harness.rpc<WriteView>('EditTrackerSprint', board.connectionId, { board: BOARD_ID, sprint: SPRINT_ID, name: 'Sprint 1, renamed' });
  expect(sprint.parts.map((p) => [p.step, p.state])).toEqual([['sprint', 'applied']]);
  const read = await fetch(`${board.jiraURL}/rest/agile/1.0/sprint/${SPRINT_ID}`, { headers: auth });
  expect(((await read.json()) as { name: string }).name).toBe('Sprint 1, renamed');
});
