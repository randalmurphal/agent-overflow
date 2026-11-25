// An issue tracker board over the wire, from a client to the fake Jira
// and back: a client subscribes to a board its project lists, its first
// read arrives as tracker:board_updated, the board and a section read
// from this computer's copy, an issue renamed in Jira reaches the client
// as an event that names it and a patch that shows the new title, and a
// release ends the subscription. A client whose socket closes has its
// subscription released with it, so its board is no longer read for it.
//
// Who receives tracker:board_updated (a session holding tracker:read)
// is the channel's policy row, proved by event_scope_test.go; the
// release on a closed connection ending the board's pump is
// TestTrackerBoardSubscriptionsFollowTheirClient (internal/app).

import { test, expect } from './fixtures.js';
import { answered, WireClient } from './offhost-helpers.js';
import { BOARD_ID, SPRINT_ID, connectFakeJiraBoard, renameJiraIssue } from './tracker-helpers.js';

interface BoardSubscription { id: string; seq: number; loaded: boolean }
interface BoardEvent {
  scope: { connection: string; board: string };
  seq: number;
  changed?: string[];
  read_again?: boolean;
  ended?: boolean;
}
interface RowView { card: { id: string; key: string; title: string } }
interface SectionPage { rows: RowView[] }
interface BoardView { name: string; lifetime: number; totals_revision: number; sections: SectionPage[] }
interface BoardPatch { sections: SectionPage[] }

const sprint = [{ kind: 'sprint', sprint: SPRINT_ID }];

test('a tracker board reaches its client and follows a change made in Jira', async ({ harness }) => {
  const board = await connectFakeJiraBoard(harness, 'tracker-board-wire');
  const ofBoard = (e: BoardEvent) => e.scope.connection === board.connectionId && e.scope.board === BOARD_ID;

  const sub = await harness.rpc<BoardSubscription>('SubscribeTrackerBoard', board.projectId, board.connectionId, BOARD_ID, sprint, true);
  expect(sub.loaded, 'a board this computer never read is not loaded at its first subscribe').toBe(false);
  await harness.waitForEvent<BoardEvent>('tracker:board_updated', (e) => ofBoard(e) && e.read_again === true && e.seq > sub.seq);

  const view = await harness.rpc<BoardView>('TrackerBoard', sub.id, sprint);
  expect(view.name).toBe('WEB board');
  expect(view.sections[0].rows.map((r) => r.card.key).sort()).toEqual(['WEB-1', 'WEB-2']);
  const backlog = await harness.rpc<SectionPage>('TrackerSectionRows', sub.id, { kind: 'backlog' }, null);
  expect(backlog.rows.map((r) => r.card.key)).toEqual(['WEB-3']);

  await renameJiraIssue(board, 'WEB-2', 'Second task, renamed in Jira');
  await harness.rpc('RefreshTrackerSubscription', sub.id);
  const changed = await harness.waitForEvent<BoardEvent>('tracker:board_updated', (e) => ofBoard(e) && (e.changed ?? []).includes('10102'));
  const patch = await harness.rpc<BoardPatch>('TrackerBoardPatch', sub.id, sprint, changed.changed, {
    lifetime: view.lifetime, revision: view.totals_revision,
  });
  expect(patch.sections[0].rows.map((r) => [r.card.key, r.card.title])).toEqual([['WEB-2', 'Second task, renamed in Jira']]);

  await harness.rpc('ReleaseTrackerSubscription', sub.id);
  await expect(harness.rpc('TrackerBoard', sub.id, sprint)).rejects.toThrow(/^tracker_subscription_unknown: /);
});

test('a client whose socket closes has its board subscription released', async ({ harness }) => {
  const board = await connectFakeJiraBoard(harness, 'tracker-board-close');
  const client = await WireClient.connect('127.0.0.1', harness.bootstrap.port, new URLSearchParams({ token: harness.bootstrap.token }).toString());
  const sub = answered(
    await client.call('SubscribeTrackerBoard', board.projectId, board.connectionId, BOARD_ID, sprint, true),
    'the second client subscribes to the board',
  ) as BoardSubscription;
  // The subscription is live: a refresh by its ID reads the fake Jira.
  const before = (await harness.rpc<{ requests: { seq: number }[] }>('HarnessTrackerRequests', 0)).requests.length;
  await harness.rpc('RefreshTrackerSubscription', sub.id);
  const after = (await harness.rpc<{ requests: { seq: number }[] }>('HarnessTrackerRequests', 0)).requests.length;
  expect(after, 'a refresh of a live subscription reads the tracker').toBeGreaterThan(before);

  client.close();
  await expect.poll(
    () => harness.rpc('RefreshTrackerSubscription', sub.id).then(() => 'live', (err: Error) => err.message),
    { message: 'the closed socket released its subscription' },
  ).toMatch(/^tracker_subscription_unknown: /);
});
