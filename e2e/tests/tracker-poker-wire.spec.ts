// Planning poker over the wire, from a client to the fake Jira site's
// add-on and back: a client asks whether the site has it, follows a
// subscribed ticket's votes and gets their first read as
// tracker:issue_updated with poker set, votes, reveals and clears, each
// shown with no refresh, a vote request with no value is refused and the
// vote cast stays, and no event or request the fake logged carries
// the add-on's context token. A site without the add-on answers that it
// has none, and refuses a vote with a sentence.
//
// The follow's interval, a hidden ticket's cost, other voters and the
// token kept in memory only are TestPoker* in internal/trackerapp.

import { test, expect } from './fixtures.js';
import { connectFakeJiraBoard } from './tracker-helpers.js';

interface PokerVote { voter: { id: string; name?: string }; value?: number; mine?: boolean }
interface PokerView { loaded: boolean; revealed?: boolean; votes?: PokerVote[]; failure?: { message: string } }
interface IssueEvent { connection: string; issue: string; poker?: boolean }
interface WriteView { parts: { step: string; state: string; refusal?: { reason: string; message: string } }[] }

// A JWT's header and claims, as base64url JSON objects: the shape of the
// add-on's context token.
const jwt = /eyJ[\w-]+\.eyJ[\w-]+\./;

test('planning poker reaches its client: read, vote, reveal and clear', async ({ harness }) => {
  const board = await connectFakeJiraBoard(harness, 'tracker-poker-wire', { poker: {} });
  const poker = () => harness.rpc<PokerView>('TrackerPoker', sub.id);

  expect(await harness.rpc('TrackerPokerSupport', board.connectionId)).toEqual({ available: true });
  const sub = await harness.rpc<{ id: string }>('SubscribeTrackerTicket', board.connectionId, '10101');
  const followed = await harness.rpc<PokerView>('FollowTrackerPoker', sub.id, true);
  expect(followed.loaded).toBe(false);
  await harness.waitForEvent<IssueEvent>('tracker:issue_updated', (e) => e.issue === '10101' && e.poker === true);
  expect(await poker()).toMatchObject({ loaded: true });
  expect((await poker()).votes ?? []).toEqual([]);

  const vote = await harness.rpc<WriteView>('VoteTrackerPoker', board.connectionId, { issue: '10101', vote: 5 });
  expect(vote.parts.map((p) => [p.step, p.state])).toEqual([['poker', 'applied']]);
  await expect.poll(async () => (await poker()).votes, { message: 'the ticket shows my vote' })
    .toEqual([{ voter: expect.objectContaining({ id: 'acc-user1', name: 'User 1' }), value: 5, mine: true }]);

  // A request with no vote, or a null one, is refused before anything is
  // sent, and the vote cast stays.
  await expect(harness.rpc('VoteTrackerPoker', board.connectionId, { issue: '10101' })).rejects.toThrow(/^tracker_request_invalid: /);
  await expect(harness.rpc('VoteTrackerPoker', board.connectionId, { issue: '10101', vote: null })).rejects.toThrow(/^tracker_request_invalid: /);
  expect((await poker()).votes?.map((v) => v.value)).toEqual([5]);

  const reveal = await harness.rpc<WriteView>('RevealTrackerPoker', board.connectionId, '10101');
  expect(reveal.parts.map((p) => p.state)).toEqual(['applied']);
  await expect.poll(async () => (await poker()).revealed, { message: 'the ticket shows the reveal' }).toBe(true);

  const clear = await harness.rpc<WriteView>('ClearTrackerPokerVotes', board.connectionId, '10101');
  expect(clear.parts.map((p) => p.state)).toEqual(['applied']);
  await expect.poll(async () => {
    const v = await poker();
    return [v.revealed ?? false, v.votes?.length ?? 0];
  }, { message: 'the ticket shows no votes' }).toEqual([false, 0]);

  expect(harness.countEvents<IssueEvent>('tracker:issue_updated', (e) => jwt.test(JSON.stringify(e))), 'an event carries a token').toBe(0);
  const log = await harness.rpc<{ requests: unknown[] }>('HarnessTrackerRequests', 0);
  expect(log.requests.filter((r) => jwt.test(JSON.stringify(r))), 'the fake logged a token').toEqual([]);

  await harness.rpc('ReleaseTrackerSubscription', sub.id);
  await expect(harness.rpc('TrackerPoker', sub.id)).rejects.toThrow(/^tracker_subscription_unknown: /);
});

test('a Jira site without the add-on has no planning poker', async ({ harness }) => {
  const board = await connectFakeJiraBoard(harness, 'tracker-poker-absent');
  expect(await harness.rpc('TrackerPokerSupport', board.connectionId)).toEqual({ available: false });
  const vote = await harness.rpc<WriteView>('VoteTrackerPoker', board.connectionId, { issue: '10101', vote: 5 });
  expect(vote.parts).toEqual([expect.objectContaining({
    step: 'poker', state: 'refused',
    refusal: expect.objectContaining({ reason: 'unsupported', message: 'planning poker (Easy Agile TeamRhythm) is not installed on this Jira site' }),
  })]);
});
