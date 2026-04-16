import type { Settings } from '../types/settings';

type Theme = Settings['theme'];

let mediaQuery: MediaQueryList | null = null;
let currentListener: (() => void) | null = null;

function resolveSystemTheme(): 'light' | 'dark' {
  if (!mediaQuery) {
    mediaQuery = window.matchMedia('(prefers-color-scheme: dark)');
  }
  return mediaQuery.matches ? 'dark' : 'light';
}

function setHtmlClass(resolved: 'light' | 'dark'): void {
  document.documentElement.classList.remove('light', 'dark');
  document.documentElement.classList.add(resolved);
}

export function applyTheme(theme: Theme): void {
  // Clean up previous system listener
  if (currentListener && mediaQuery) {
    mediaQuery.removeEventListener('change', currentListener);
    currentListener = null;
  }

  if (theme === 'system') {
    setHtmlClass(resolveSystemTheme());
    currentListener = () => setHtmlClass(resolveSystemTheme());
    if (!mediaQuery) {
      mediaQuery = window.matchMedia('(prefers-color-scheme: dark)');
    }
    mediaQuery.addEventListener('change', currentListener);
  } else {
    setHtmlClass(theme);
  }
}
