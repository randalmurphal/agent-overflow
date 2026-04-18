import { describe, expect, it, beforeEach } from 'vitest';
import {
  closeThreadPicker,
  isThreadPickerOpen,
  openThreadPicker,
  toggleThreadPicker,
} from './threadPicker.svelte';

describe('threadPicker store', () => {
  beforeEach(() => {
    // Ensure a clean slate between tests — module-scoped $state persists.
    closeThreadPicker();
  });

  it('starts closed', () => {
    expect(isThreadPickerOpen()).toBe(false);
  });

  it('openThreadPicker flips to true, closeThreadPicker flips back', () => {
    openThreadPicker();
    expect(isThreadPickerOpen()).toBe(true);
    closeThreadPicker();
    expect(isThreadPickerOpen()).toBe(false);
  });

  it('toggleThreadPicker alternates between open and closed', () => {
    expect(isThreadPickerOpen()).toBe(false);
    toggleThreadPicker();
    expect(isThreadPickerOpen()).toBe(true);
    toggleThreadPicker();
    expect(isThreadPickerOpen()).toBe(false);
  });

  it('openThreadPicker is idempotent', () => {
    openThreadPicker();
    openThreadPicker();
    expect(isThreadPickerOpen()).toBe(true);
  });

  it('closeThreadPicker is idempotent', () => {
    closeThreadPicker();
    closeThreadPicker();
    expect(isThreadPickerOpen()).toBe(false);
  });
});
