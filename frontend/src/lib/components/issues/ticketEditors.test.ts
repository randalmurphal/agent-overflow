// A list held here (heldChoices) as its picker reads it: one search at
// every size, bounded before any value is made into a choice.

import { describe, expect, it } from 'vitest';
import { heldChoices, PICK_LIMIT, type HeldList } from './ticketEditors';

interface Item { readonly id: string; readonly name: string }

function list(): HeldList<Item> & { made: number } {
  const out = {
    made: 0,
    searched: [(v: Item) => v.name],
    choice: (v: Item) => {
      out.made += 1;
      return { id: v.id, name: v.name };
    },
  };
  return out;
}

const ITEMS: Item[] = Array.from({ length: 300 }, (_, i) => ({ id: String(i), name: `Item ${i}` }));

describe('heldChoices', () => {
  it('answers the first PICK_LIMIT matches with more, making choices of those alone', () => {
    const l = list();
    const got = heldChoices(ITEMS, '', l);
    expect(got.choices).toHaveLength(PICK_LIMIT);
    expect(got.more).toBe(true);
    expect(l.made).toBe(PICK_LIMIT);
  });

  it('says no more at exactly PICK_LIMIT matches, and more at one past', () => {
    expect(heldChoices(ITEMS.slice(0, PICK_LIMIT), '', list()).more).toBe(false);
    expect(heldChoices(ITEMS.slice(0, PICK_LIMIT + 1), '', list()).more).toBe(true);
  });

  it('finds text anywhere in a value, in any case, past the bound', () => {
    const got = heldChoices(ITEMS, ' TEM 29 ', list());
    expect(got.choices.map((c) => c.name)).toEqual(['Item 29', ...Array.from({ length: 10 }, (_, i) => `Item 29${i}`)]);
    expect(got.more).toBe(false);
  });
});
