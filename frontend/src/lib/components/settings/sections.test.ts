import { describe, expect, it } from 'vitest';
import { settingsUsesComputer } from './sections';

describe('settingsUsesComputer', () => {
  // Each computer has its own Assistants switch: the page follows the
  // computer picked at the top of Settings.
  it('gives the Assistants page the computer picker', () => {
    expect(settingsUsesComputer('assistants')).toBe(true);
  });
});
