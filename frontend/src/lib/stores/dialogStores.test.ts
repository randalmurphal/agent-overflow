// Three near-identical open/close stores — palette, cheatSheet,
// messageSearch — share a single test file because the shape is
// identical and duplicating one test file per store is just noise.

import { describe, expect, it, beforeEach } from 'vitest';
import {
  closeCheatSheet,
  isCheatSheetOpen,
  openCheatSheet,
  toggleCheatSheet,
} from './cheatSheet.svelte';
import {
  closeMessageSearch,
  isMessageSearchOpen,
  openMessageSearch,
  toggleMessageSearch,
} from './messageSearch.svelte';
import {
  closePalette,
  isPaletteOpen,
  openPalette,
  togglePalette,
} from './palette.svelte';

describe.each([
  {
    name: 'cheatSheet',
    isOpen: isCheatSheetOpen,
    open: openCheatSheet,
    close: closeCheatSheet,
    toggle: toggleCheatSheet,
  },
  {
    name: 'messageSearch',
    isOpen: isMessageSearchOpen,
    open: openMessageSearch,
    close: closeMessageSearch,
    toggle: toggleMessageSearch,
  },
  {
    name: 'palette',
    isOpen: isPaletteOpen,
    open: openPalette,
    close: closePalette,
    toggle: togglePalette,
  },
])('$name store', ({ isOpen, open, close, toggle }) => {
  beforeEach(() => {
    // Each store is module-scoped singleton state — reset to closed
    // before each test so leftover state from other tests (or from the
    // previous iteration of describe.each) doesn't contaminate.
    close();
  });

  it('starts closed', () => {
    expect(isOpen()).toBe(false);
  });

  it('open() flips open', () => {
    open();
    expect(isOpen()).toBe(true);
  });

  it('close() flips closed', () => {
    open();
    close();
    expect(isOpen()).toBe(false);
  });

  it('toggle() cycles', () => {
    toggle();
    expect(isOpen()).toBe(true);
    toggle();
    expect(isOpen()).toBe(false);
  });

  it('open() is idempotent', () => {
    open();
    open();
    expect(isOpen()).toBe(true);
  });

  it('close() is idempotent', () => {
    close();
    close();
    expect(isOpen()).toBe(false);
  });
});
