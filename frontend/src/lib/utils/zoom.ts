// Scales all rem-based text by adjusting the root font-size on <html>.
// The browser default is 16px; the app's body text (--text-sm) is
// 0.8125rem = 13px at that default. All text-[Npx] classes have been
// converted to rem equivalents so they respond to this.

import { getSettings, updateSetting } from '../stores/settings.svelte';

// Must match internal/settings.DefaultSettings.FontSize and
// internal/settings.{Min,Max}FontSize.
const BODY_TEXT_PX = 13;
const MIN_FONT_SIZE = 10;
const MAX_FONT_SIZE = 20;
const BROWSER_DEFAULT_ROOT_PX = 16;

let lastFontSize: number | null = null;

export function applyFontScale(fontSize: number): void {
  if (fontSize === lastFontSize) return;
  lastFontSize = fontSize;

  if (fontSize === BODY_TEXT_PX) {
    document.documentElement.style.removeProperty('font-size');
  } else {
    const rootPx = (fontSize * BROWSER_DEFAULT_ROOT_PX) / BODY_TEXT_PX;
    document.documentElement.style.setProperty('font-size', `${rootPx}px`);
  }
}

function adjustFontSize(delta: number): void {
  const current = getSettings().fontSize;
  const next = Math.max(MIN_FONT_SIZE, Math.min(MAX_FONT_SIZE, current + delta));
  if (next !== current) {
    void updateSetting('fontSize', next);
  }
}

function handleZoomKeydown(e: KeyboardEvent): void {
  const mod = e.metaKey || e.ctrlKey;
  if (!mod) return;

  if (e.key === '+' || e.key === '=') {
    e.preventDefault();
    adjustFontSize(1);
  } else if (e.key === '-') {
    e.preventDefault();
    adjustFontSize(-1);
  } else if (e.key === '0') {
    e.preventDefault();
    const current = getSettings().fontSize;
    if (current !== BODY_TEXT_PX) {
      void updateSetting('fontSize', BODY_TEXT_PX);
    }
  }
}

export function installZoomKeybindings(): () => void {
  window.addEventListener('keydown', handleZoomKeydown);
  return () => window.removeEventListener('keydown', handleZoomKeydown);
}
