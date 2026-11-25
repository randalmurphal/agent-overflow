// An issue tracker ticket over the wire, from a client to the fake Jira
// and back: a client subscribes to a ticket, its first read arrives as
// tracker:issue_updated, the ticket reads from memory, an issue renamed in
// Jira reaches the client on a refresh, a comment written through the app
// shows on the ticket with no refresh, a file attached through the
// ticketed upload is saved back byte for byte through the ticketed
// download, and a release ends the subscription. A client whose socket
// closes has its ticket subscription released with it.
//
// Who receives tracker:issue_updated (a session holding tracker:read) is
// the channel's policy row, proved by event_scope_test.go; the release on
// a closed connection ending the ticket's pump is
// TestTrackerTicketSubscriptionsFollowTheirClient (internal/app).

import { test, expect } from './fixtures.js';
import { answered, WireClient } from './offhost-helpers.js';
import { connectFakeJiraBoard, renameJiraIssue } from './tracker-helpers.js';

interface TicketView {
  loaded: boolean;
  detail?: {
    card: { key: string; title: string };
    comments: { body: string }[];
    attachments: { id: string; name: string; size: number }[];
  };
}
interface TicketSubscription extends TicketView { id: string; seq: number }
interface IssueEvent { connection: string; issue: string; seq: number; changed?: boolean; ended?: boolean }
interface WriteView { parts: { issue: string; step: string; state: string }[] }

test('a tracker ticket reaches its client, takes a comment and a file, and saves the file back', async ({ harness }) => {
  const board = await connectFakeJiraBoard(harness, 'tracker-ticket-wire');
  const base = `http://127.0.0.1:${harness.bootstrap.port}`;
  const ofTicket = (e: IssueEvent) => e.connection === board.connectionId && e.issue === '10101';
  const ticket = () => harness.rpc<TicketView>('TrackerTicket', sub.id);

  const sub = await harness.rpc<TicketSubscription>('SubscribeTrackerTicket', board.connectionId, '10101', true);
  await harness.waitForEvent<IssueEvent>('tracker:issue_updated', (e) => ofTicket(e) && e.changed === true && e.seq > sub.seq);
  expect((await ticket()).detail?.card.title).toBe('First story');

  await renameJiraIssue(board, 'WEB-1', 'First story, renamed in Jira');
  await harness.rpc('RefreshTrackerSubscription', sub.id);
  await expect.poll(async () => (await ticket()).detail?.card.title, { message: 'the refreshed ticket shows the new title' })
    .toBe('First story, renamed in Jira');

  const comment = await harness.rpc<WriteView>('AddTrackerComment', board.connectionId, { issue: '10101', markdown: 'Looks good from here' });
  expect(comment.parts.map((p) => p.state)).toEqual(['applied']);
  expect((await ticket()).detail?.comments.map((c) => c.body).join('\n')).toContain('Looks good from here');

  const file = Buffer.from('line one\nline two\n'.repeat(4096));
  const upload = await harness.rpc<string>('MintTrackerAttachmentUploadTicket', board.connectionId, '10101', 'notes.txt', file.length);
  const put = await fetch(base + upload, { method: 'PUT', body: file });
  expect(put.status).toBe(200);
  const attached = await put.json() as WriteView;
  expect(attached.parts.map((p) => [p.step, p.state])).toEqual([['upload', 'applied']]);
  const attachment = (await ticket()).detail?.attachments.find((a) => a.name === 'notes.txt');
  expect(attachment, 'the ticket shows the attached file').toBeTruthy();

  const download = await harness.rpc<string>('MintTrackerAttachmentDownloadTicket', sub.id, attachment!.id);
  const got = await fetch(base + download);
  expect(got.status).toBe(200);
  expect(got.headers.get('content-type')).toBe('application/octet-stream');
  expect(got.headers.get('content-disposition')).toBe('attachment; filename=notes.txt');
  expect(Buffer.from(await got.arrayBuffer()).equals(file), 'the saved file is the attached file').toBe(true);

  await harness.rpc('ReleaseTrackerSubscription', sub.id);
  await expect(harness.rpc('TrackerTicket', sub.id)).rejects.toThrow(/^tracker_subscription_unknown: /);
});

test('a client whose socket closes has its ticket subscription released', async ({ harness }) => {
  const board = await connectFakeJiraBoard(harness, 'tracker-ticket-close');
  const client = await WireClient.connect('127.0.0.1', harness.bootstrap.port, new URLSearchParams({ token: harness.bootstrap.token }).toString());
  const sub = answered(
    await client.call('SubscribeTrackerTicket', board.connectionId, '10101', true),
    'the second client subscribes to the ticket',
  ) as TicketSubscription;
  await harness.rpc('RefreshTrackerSubscription', sub.id);

  client.close();
  await expect.poll(
    () => harness.rpc('TrackerTicket', sub.id).then(() => 'live', (err: Error) => err.message),
    { message: 'the closed socket released its subscription' },
  ).toMatch(/^tracker_subscription_unknown: /);
});
