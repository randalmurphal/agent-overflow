import { describe, expect, it } from 'vitest';
import { addressOwner, type PendingSelection } from './browserAddressOwner';

function pending(pageId: string, observed = false, superseded = false): PendingSelection {
  return { pageId, observed, superseded };
}

describe('addressOwner', () => {
  // A switch on its way to a tab the thread no longer has owns nothing,
  // whatever else is true of it: the owner is always a tab that exists.
  it.each([
    // [what happened, active tab, pending selection, tab a gone, owner]
    ['no switch pending: the tab shown', 'b', null, false, 'b'],
    ['a switch to a on its way: a', 'b', pending('a'), false, 'a'],
    ['the switch to a shown: a', 'a', pending('a', true), false, 'a'],
    ['the switch to a shown, then the user back on b: b', 'b', pending('a', true), false, 'b'],
    ['a later selection by the user before a was shown: the tab shown', 'b', pending('a', false, true), false, 'b'],
    ['shown and then superseded: the tab shown', 'c', pending('a', true, true), false, 'c'],
    ['no tab shown and none pending: none', null, null, false, null],
    ['no tab shown and a switch on its way: its tab', null, pending('a'), false, 'a'],
    ['a switch to a tab that is gone, on its way: the tab shown', 'b', pending('a'), true, 'b'],
    ['a switch to a tab that is gone, on its way, no tab shown: none', null, pending('a'), true, null],
    ['a switch to a tab that is gone, shown before it went: the tab shown', 'b', pending('a', true), true, 'b'],
    ['a switch to a tab that is gone, superseded: the tab shown', 'b', pending('a', false, true), true, 'b'],
  ] as const)('%s', (_case, active, selection, aGone, owner) => {
    expect(addressOwner(active, selection, (pageId) => !aGone || pageId !== 'a')).toBe(owner);
  });
});
