import { describe, expect, it } from 'vitest';
import { parkedStopFromItem, parkedStopSentence } from './parkedStop';

describe('parkedStopFromItem', () => {
  it('reads a parked sibling and nothing else', () => {
    const meta = JSON.stringify({ parked_commands: 2, parked_report_item_id: 'r1', run_started_at: 500, run_woke: true });
    expect(parkedStopFromItem({ status: 'parked', meta })).toEqual({ waitingOn: 2, reportItemId: 'r1', woke: true });
    expect(parkedStopFromItem({ status: 'completed', meta })).toBeNull();
    expect(parkedStopFromItem(null)).toBeNull();
  });

  it('reads a sibling with no run facts as an unwoken run with no report', () => {
    expect(parkedStopFromItem({ status: 'parked', meta: '' })).toEqual({ waitingOn: 0, reportItemId: '', woke: false });
  });
});

describe('parkedStopSentence', () => {
  const stop = { reportItemId: '' };
  it('says whether the run was woken and pluralizes the commands', () => {
    expect(parkedStopSentence({ ...stop, waitingOn: 1, woke: false })).toBe('Reported, waiting on 1 background command');
    expect(parkedStopSentence({ ...stop, waitingOn: 3, woke: true })).toBe('Reported again, waiting on 3 background commands');
    expect(parkedStopSentence({ ...stop, waitingOn: 0, woke: false })).toBe('Reported, waiting on background commands');
  });
});
