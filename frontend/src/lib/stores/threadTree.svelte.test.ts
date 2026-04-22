// Per-parent expansion set for the sidebar's nested thread view. Tests
// cover add / remove / toggle semantics plus the idempotency of
// ensureExpanded (additive union).

import { beforeEach, describe, expect, it } from 'vitest';
import {
  collapseParent,
  ensureExpanded,
  expandParent,
  getExpandedParents,
  isParentExpanded,
  resetExpandedParentsForTest,
  toggleParent,
} from './threadTree.svelte';

beforeEach(() => {
  resetExpandedParentsForTest();
});

describe('threadTree store', () => {
  it('starts empty', () => {
    expect(isParentExpanded('p1')).toBe(false);
    expect(getExpandedParents().size).toBe(0);
  });

  it('expandParent marks an id as expanded', () => {
    expandParent('p1');
    expect(isParentExpanded('p1')).toBe(true);
  });

  it('expandParent is idempotent', () => {
    expandParent('p1');
    expandParent('p1');
    expect(getExpandedParents().size).toBe(1);
  });

  it('collapseParent removes an id', () => {
    expandParent('p1');
    collapseParent('p1');
    expect(isParentExpanded('p1')).toBe(false);
  });

  it('collapseParent on an unknown id is a no-op', () => {
    expect(() => collapseParent('nope')).not.toThrow();
  });

  it('toggleParent flips the state', () => {
    toggleParent('p1');
    expect(isParentExpanded('p1')).toBe(true);
    toggleParent('p1');
    expect(isParentExpanded('p1')).toBe(false);
  });

  it('ensureExpanded unions the given ids into the set', () => {
    expandParent('a');
    ensureExpanded(['b', 'c']);
    expect(isParentExpanded('a')).toBe(true);
    expect(isParentExpanded('b')).toBe(true);
    expect(isParentExpanded('c')).toBe(true);
  });

  it('ensureExpanded is additive — never collapses', () => {
    expandParent('a');
    // ensureExpanded(['c']) should leave 'a' alone.
    ensureExpanded(['c']);
    expect(isParentExpanded('a')).toBe(true);
  });

  it('ensureExpanded with an empty iterable is a no-op', () => {
    expandParent('a');
    ensureExpanded([]);
    expect(getExpandedParents().size).toBe(1);
  });
});
