import { describe, it, expect } from 'vitest';
import { getXtermTheme } from './terminalTheme';

const REQUIRED_KEYS = [
  'background', 'foreground', 'cursor', 'cursorAccent',
  'selectionBackground', 'selectionForeground',
  'black', 'red', 'green', 'yellow', 'blue', 'magenta', 'cyan', 'white',
  'brightBlack', 'brightRed', 'brightGreen', 'brightYellow',
  'brightBlue', 'brightMagenta', 'brightCyan', 'brightWhite',
] as const;

describe('getXtermTheme', () => {
  it("returns the dark palette for 'dark'", () => {
    const t = getXtermTheme('dark') as Record<string, string>;
    for (const k of REQUIRED_KEYS) {
      expect(t[k]).toMatch(/^#[0-9a-fA-F]{3,8}$/);
    }
    expect(t.background).toBe('#1c1d24');
    expect(t.foreground).toBe('#eeeef0');
    expect(t.brightWhite).toBe('#ffffff');
  });

  it("returns the light palette for 'light'", () => {
    const t = getXtermTheme('light') as Record<string, string>;
    for (const k of REQUIRED_KEYS) {
      expect(t[k]).toMatch(/^#[0-9a-fA-F]{3,8}$/);
    }
    expect(t.background).toBe('#fafafb');
    expect(t.foreground).toBe('#34373d');
    // Light "white" should NOT be #ffffff (would be invisible).
    expect(t.white).not.toBe('#ffffff');
    expect(t.brightWhite).not.toBe('#ffffff');
  });

  it('returns objects that differ by mode', () => {
    const dark = getXtermTheme('dark');
    const light = getXtermTheme('light');
    expect(dark.background).not.toBe(light.background);
    expect(dark.foreground).not.toBe(light.foreground);
    expect(dark.red).not.toBe(light.red);
  });
});
