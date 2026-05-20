// xterm.js theme objects keyed by resolved app theme. xterm's ITheme
// only accepts hex/rgb strings — passing the oklch values from
// app.css's dark palette would silently fall back to #000, so we keep
// a parallel hex-only map of the dark ANSI colors here. The light
// palette in app.css is already hex; we still keep it inline so the
// terminal theme is self-contained and trivially testable.
//
// Chat ANSI (.ansi-body .ansi-fg-N) and terminal output are
// aesthetically twinned, not byte-identical. Subtle drift between
// the dark oklch values in app.css and the hex equivalents below is
// expected.

import type { ITheme } from '@xterm/xterm';
import type { ResolvedTheme } from '../../stores/themeMode.svelte';

const DARK = {
  background: '#000000',
  foreground: '#eeeef0',           // ≈ --text-primary dark
  cursor: '#eeeef0',
  cursorAccent: '#000000',
  selectionBackground: '#3a3a48',
  selectionForeground: '#eeeef0',
  black: '#4b5563',
  red: '#ef4444',
  green: '#22c55e',
  yellow: '#eab308',
  blue: '#60a5fa',
  magenta: '#c084fc',
  cyan: '#67e8f9',
  white: '#f3f4f6',
  brightBlack: '#9ca3af',
  brightRed: '#f87171',
  brightGreen: '#4ade80',
  brightYellow: '#fde047',
  brightBlue: '#93c5fd',
  brightMagenta: '#d8b4fe',
  brightCyan: '#a5f3fc',
  brightWhite: '#ffffff',
} as const satisfies ITheme;

const LIGHT = {
  background: '#fafafb',           // ≈ --surface-0 light
  foreground: '#34373d',           // ≈ --text-primary light
  cursor: '#34373d',
  cursorAccent: '#fafafb',
  selectionBackground: '#c7d2fe',
  selectionForeground: '#1f2328',
  black: '#24292f',
  red: '#cf222e',
  green: '#116329',
  yellow: '#7d4e00',
  blue: '#0550ae',
  magenta: '#8250df',
  cyan: '#1b7c83',
  white: '#57606a',
  brightBlack: '#6e7781',
  brightRed: '#a40e26',
  brightGreen: '#1a7f37',
  brightYellow: '#633c01',
  brightBlue: '#0969da',
  brightMagenta: '#6f42c1',
  brightCyan: '#3192aa',
  brightWhite: '#1f2328',
} as const satisfies ITheme;

export function getXtermTheme(mode: ResolvedTheme): ITheme {
  return mode === 'light' ? LIGHT : DARK;
}
