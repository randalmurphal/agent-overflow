import type { MonoFont, SansFont } from '../types/settings';

/**
 * `geist` is intentionally absent from these tables. The eager Geist
 * stack is declared once in app.css's `@theme` block; selecting Geist
 * here calls `removeProperty` so the cascade-defined value wins. That
 * keeps the fallback chain in exactly one place — adding a future
 * Geist weight or reordering the chain only edits app.css.
 */
const SANS_OVERRIDES: Partial<Record<SansFont, string>> = {
  // Front the mono fallback chain so the lazy-load FOUT period stays
  // monospace instead of swapping back to a sans face mid-load.
  'hack-nerd':
    "'Hack Nerd Font', 'Geist Mono', ui-monospace, SFMono-Regular, Consolas, 'Liberation Mono', Menlo, monospace",
  system: 'ui-sans-serif, system-ui, -apple-system, "Segoe UI", sans-serif',
};

const MONO_OVERRIDES: Partial<Record<MonoFont, string>> = {
  'hack-nerd':
    "'Hack Nerd Font', 'Geist Mono', ui-monospace, SFMono-Regular, Consolas, 'Liberation Mono', Menlo, monospace",
  system: 'ui-monospace, SFMono-Regular, Consolas, "Liberation Mono", Menlo, monospace',
};

let hackLoaded: Promise<unknown> | null = null;

function ensureHackLoaded(): Promise<unknown> {
  if (!hackLoaded) {
    hackLoaded = import('./hack-nerd.css').catch((err) => {
      // Reset so a future selection re-tries instead of permanently
      // wedging on a transient fetch failure.
      hackLoaded = null;
      throw err;
    });
  }
  return hackLoaded;
}

let lastSans: SansFont | null = null;
let lastMono: MonoFont | null = null;

/**
 * Apply font choices to the document. Mirrors applyTheme() but writes
 * CSS variables instead of toggling classes — fonts are a single
 * string per family, so there's no peer set of variables to swap like
 * there is for the light/dark `:root` blocks in app.css.
 *
 * Synchronous: writes the CSS variables immediately so the fallback
 * chain renders without waiting on the Hack Nerd Font import. The
 * lazy import is kicked off in the background; once it resolves, the
 * @font-face declarations land and the browser swaps in Hack via
 * font-display: swap.
 */
export function applyFonts(sans: SansFont, mono: MonoFont): void {
  if (sans === lastSans && mono === lastMono) return;
  lastSans = sans;
  lastMono = mono;

  if (sans === 'hack-nerd' || mono === 'hack-nerd') {
    ensureHackLoaded().catch((err) => {
      console.error('Failed to load Hack Nerd Font:', err);
    });
  }

  applyVar('--font-sans', SANS_OVERRIDES[sans]);
  applyVar('--font-mono', MONO_OVERRIDES[mono]);
}

function applyVar(name: '--font-sans' | '--font-mono', value: string | undefined): void {
  if (value) {
    document.documentElement.style.setProperty(name, value);
  } else {
    document.documentElement.style.removeProperty(name);
  }
}
