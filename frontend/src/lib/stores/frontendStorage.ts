// Preferences owned by this frontend, independent of any paired computer.
// These values survive forgetting the first host and remain writable while
// every host is offline. Never store credentials here.
import { addToast } from './toast.svelte';

const PREFIX = 'agent-overflow:frontend:';
let failureShown = false;

export function readFrontendValue(key: string): unknown {
  try {
    const raw = localStorage.getItem(PREFIX + key);
    return raw === null ? null : JSON.parse(raw);
  } catch {
    return null;
  }
}

export function writeFrontendValue(key: string, value: unknown): boolean {
  try {
    localStorage.setItem(PREFIX + key, JSON.stringify(value));
    failureShown = false;
    return true;
  } catch {
    if (!failureShown) {
      failureShown = true;
      addToast('error', 'This device could not save its preferences. Changes may be lost when the app closes.');
    }
    return false;
  }
}

/** Other windows of this frontend share preferences through the browser. */
export function onFrontendValueChanged(key: string, changed: () => void): () => void {
  const receive = (event: StorageEvent) => {
    if (event.storageArea !== localStorage) return;
    if (event.key === null || event.key === PREFIX + key) changed();
  };
  window.addEventListener('storage', receive);
  return () => window.removeEventListener('storage', receive);
}
