// Near-identical open/close stores — palette, cheatSheet, messageSearch,
// threadPicker, accountSwitcher — share a single test file because the shape
// is identical and duplicating one test file per store is just noise.

import { describe, expect, it, beforeEach } from 'vitest';
import {
  closeCheatSheet,
  isCheatSheetOpen,
  openCheatSheet,
} from './cheatSheet.svelte';
import {
  closeMessageSearch,
  isMessageSearchOpen,
  openMessageSearch,
} from './messageSearch.svelte';
import {
  closePalette,
  isPaletteOpen,
  openPalette,
} from './palette.svelte';
import {
  closeThreadPicker,
  isThreadPickerOpen,
  openThreadPicker,
} from './threadPicker.svelte';
import {
  closeAccountSwitcher,
  isAccountSwitcherOpen,
  toggleAccountSwitcher,
} from './accountSwitcher.svelte';

describe.each([
  {
    name: 'cheatSheet',
    isOpen: isCheatSheetOpen,
    open: openCheatSheet,
    close: closeCheatSheet,
  },
  {
    name: 'messageSearch',
    isOpen: isMessageSearchOpen,
    open: openMessageSearch,
    close: closeMessageSearch,
  },
  {
    name: 'palette',
    isOpen: isPaletteOpen,
    open: openPalette,
    close: closePalette,
  },
  {
    name: 'threadPicker',
    isOpen: isThreadPickerOpen,
    open: openThreadPicker,
    close: closeThreadPicker,
  },
])('$name store', ({ isOpen, open, close }) => {
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

describe('accountSwitcher store', () => {
  beforeEach(() => {
    closeAccountSwitcher();
  });

  it('starts closed', () => {
    expect(isAccountSwitcherOpen()).toBe(false);
  });

  it('toggle() cycles and close() closes', () => {
    toggleAccountSwitcher();
    expect(isAccountSwitcherOpen()).toBe(true);
    toggleAccountSwitcher();
    expect(isAccountSwitcherOpen()).toBe(false);
    toggleAccountSwitcher();
    closeAccountSwitcher();
    expect(isAccountSwitcherOpen()).toBe(false);
  });
});
