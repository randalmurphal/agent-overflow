// Scales all rem-based text by adjusting the root font-size on <html>.
// The browser default is 16px; the app's body text (--text-sm) is
// 0.8125rem = 13px at that default. All text-[Npx] classes have been
// converted to rem equivalents so they respond to this.

// Must match internal/settings.DefaultSettings.FontSize.
const BODY_TEXT_PX = 13;
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
