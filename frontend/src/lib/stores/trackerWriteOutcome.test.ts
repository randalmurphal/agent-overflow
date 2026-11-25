// What each write answer means for the person, as a table, and the one
// send: pinned to the connection's computer, a dropped connection as lost.

import { describe, expect, it } from 'vitest';
import type { WriteView } from '../../../bindings/agent-overflow/internal/trackerapp/models';
import { getPinnedBackend } from '../transport/backends';
import { DisconnectedError, TransportError } from '../transport/wsClient';
import { LossConsent, refusalSentence, sendWrite, TICKET_SUBJECT, writeOutcome, type WriteOutcome, type WriteSubject } from './trackerWriteOutcome';

const failure = (message: string) => ({ kind: 'transient', message, at: 1 });
const WEEK: WriteSubject = { look: "the day's logged time", shows: 'The week', ended: 'the week no longer shows its time', reads: (r) => r.kind === 'worklogs' };

describe('writeOutcome', () => {
  const cases: Array<{ name: string; view: Partial<WriteView>; want: unknown }> = [
    { name: 'every part applied', view: { parts: [{ step: 'edit', state: 'applied' }] }, want: { kind: 'applied', note: null } },
    {
      name: 'applied, with what the mirror could not show yet',
      view: { parts: [{ step: 'edit', state: 'applied' }], stale: [{ resources: [{ kind: 'detail', issue: '1' }], failure: failure('Jira did not answer in time') }] },
      want: { kind: 'applied', note: 'Saved. The ticket shows it after its next read: Jira did not answer in time' },
    },
    {
      name: 'applied while the connection was turned off',
      view: { parts: [{ step: 'comment', state: 'applied' }], ended: true },
      want: { kind: 'applied', note: 'Saved. Its connection was turned off or removed while saving, so this ticket no longer follows it.' },
    },
    {
      name: 'a part refused',
      view: { parts: [{ step: 'edit', state: 'refused', refusal: { reason: 'tracker', message: 'Type cannot change' } }] },
      want: { kind: 'refused', refusal: { reason: 'tracker', message: 'Type cannot change' } },
    },
    {
      name: 'a part sent and not answered in full',
      view: { parts: [{ step: 'move', state: 'unknown', failure: failure('the change may or may not have been made; look at the ticket before you try again') }] },
      want: { kind: 'unknown', message: 'the change may or may not have been made; look at the ticket before you try again' },
    },
    {
      name: 'a part not answered that says nothing of it',
      view: { parts: [{ step: 'move', state: 'unknown' }] },
      want: { kind: 'unknown', message: 'The tracker did not say what became of this change; look at the ticket before you try again.' },
    },
    {
      name: 'a part not sent',
      view: { parts: [{ step: 'edit', state: 'not_sent', failure: failure('nothing was sent: the connection is off') }] },
      want: { kind: 'not_sent', message: 'nothing was sent: the connection is off' },
    },
    {
      name: 'the first part that was not applied decides',
      view: { parts: [{ step: 'edit', state: 'applied' }, { step: 'edit', state: 'unknown', failure: failure('lost') }, { step: 'edit', state: 'refused', refusal: { reason: 'tracker', message: 'no' } }] },
      want: { kind: 'unknown', message: 'lost' },
    },
  ];
  for (const c of cases) {
    it(c.name, () => {
      expect(writeOutcome(c.view as WriteView, TICKET_SUBJECT)).toEqual(c.want);
    });
  }

  it('names what the screen shows the write in', () => {
    expect(writeOutcome({ parts: [{ step: 'worklog', state: 'unknown' }] } as WriteView, WEEK))
      .toEqual({ kind: 'unknown', message: "The tracker did not say what became of this change; look at the day's logged time before you try again." });
    expect(writeOutcome({ parts: [{ step: 'worklog', state: 'applied' }], stale: [{ resources: [{ kind: 'worklogs', author: 'a', day: '2026-11-10' }], failure: failure('Tempo did not answer in time') }] } as WriteView, WEEK))
      .toEqual({ kind: 'applied', note: 'Saved. The week shows it after its next read: Tempo did not answer in time' });
    expect(writeOutcome({ parts: [{ step: 'worklog', state: 'applied' }], ended: true } as WriteView, WEEK))
      .toEqual({ kind: 'applied', note: 'Saved. Its connection was turned off or removed while saving, so the week no longer shows its time.' });
  });

  it('says nothing of a read after the write of what the screen does not show', () => {
    const detail = { resources: [{ kind: 'detail', issue: '1' }], failure: failure('Jira did not answer in time') };
    const day = { resources: [{ kind: 'worklogs', author: 'a', day: '2026-11-10' }], failure: failure('Tempo did not answer in time') };
    expect(writeOutcome({ parts: [{ step: 'worklog', state: 'applied' }], stale: [detail] } as WriteView, WEEK)).toEqual({ kind: 'applied', note: null });
    expect(writeOutcome({ parts: [{ step: 'worklog', state: 'applied' }], stale: [detail, day] } as WriteView, WEEK))
      .toEqual({ kind: 'applied', note: 'Saved. The week shows it after its next read: Tempo did not answer in time' });
  });

  it('words a refusal with each field by its name and what a rich text edit would lose', () => {
    expect(refusalSentence({ reason: 'tracker', message: 'Fix these fields.', fields: [{ field: 'customfield_1', message: 'is required' }] }, () => 'Team'))
      .toBe('Fix these fields. Team: is required');
    expect(refusalSentence({ reason: 'loss', message: 'This edit would lose formatting.', lost: ['a table', 'a mention'] }, (id) => id))
      .toBe('This edit would lose formatting. It would lose: a table, a mention.');
  });
});

describe('sendWrite', () => {
  it('sends once, pinned to the computer that holds the connection', async () => {
    const pins: (string | null)[] = [];
    const outcome = await sendWrite('peer-1', 'Laptop', WEEK, async () => {
      pins.push(getPinnedBackend());
      return { parts: [{ step: 'worklog', state: 'applied' }] } as WriteView;
    });
    expect(pins).toEqual(['peer-1']);
    expect(outcome).toEqual({ kind: 'applied', note: null });
  });

  it('says an answer a dropped connection lost may or may not have been made', async () => {
    const outcome = await sendWrite('peer-1', 'Laptop', WEEK, async () => { throw new DisconnectedError('client closed'); });
    expect(outcome).toEqual({
      kind: 'lost',
      message: "Laptop: the connection dropped before the answer, so the change may or may not have been made; look at the day's logged time before you try again.",
    });
  });

  it('keeps any other failure as the call\'s error', async () => {
    const err = new TransportError('tracker_disabled', 'the connection is turned off');
    expect(await sendWrite('peer-1', 'Laptop', WEEK, async () => { throw err; })).toEqual({ kind: 'failed', error: err });
  });
});

describe('LossConsent', () => {
  const LOSS: WriteOutcome = { kind: 'refused', refusal: { reason: 'loss', message: 'This change would lose formatting.' } };

  it('holds an entry for a target only while a send of it is out', () => {
    const consent = new LossConsent(() => {});
    consent.revise('a');
    expect(consent.size).toBe(0);
    consent.sending('a');
    expect(consent.size).toBe(1);
    expect(consent.landed('a', LOSS, false)).toBe(true);
    expect(consent.size).toBe(0);
    consent.sending('b');
    consent.revise('b');
    expect(consent.landed('b', LOSS, false)).toBe(false);
    expect(consent.size).toBe(0);
  });
});
