import { describe, expect, it } from 'vitest';
import { addressOwner, type PendingSelection } from './browserAddressOwner';

function pending(pageId: string, observed = false, superseded = false): PendingSelection {
  return { pageId, observed, superseded };
}

describe('addressOwner', () => {
  it.each([
    // [what happened, active tab, pending selection, owner]
    ['no switch pending: the tab shown', 'b', null, 'b'],
    ['a switch to a on its way: a', 'b', pending('a'), 'a'],
    ['the switch to a shown: a', 'a', pending('a', true), 'a'],
    ['the switch to a shown, then the user back on b: b', 'b', pending('a', true), 'b'],
    ['a later selection by the user before a was shown: the tab shown', 'b', pending('a', false, true), 'b'],
    ['shown and then superseded: the tab shown', 'c', pending('a', true, true), 'c'],
    ['no tab shown and none pending: none', null, null, null],
    ['no tab shown and a switch on its way: its tab', null, pending('a'), 'a'],
  ] as const)('%s', (_case, active, selection, owner) => {
    expect(addressOwner(active, selection)).toBe(owner);
  });
});
