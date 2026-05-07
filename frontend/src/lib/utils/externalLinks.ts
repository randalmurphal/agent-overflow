import { OpenExternalURL } from '../stores/bindings';
import { addToast } from '../stores/toast.svelte';
import { runMode } from '../transport/runMode';
import { errString } from './errors';

let delegateInstallCount = 0;

export function safeExternalURL(raw: string | null | undefined): string | null {
  if (!raw) return null;
  const value = raw.trim();
  const schemeMatch = /^https?:\/\//.exec(value);
  if (!schemeMatch) return null;
  const authority = value.slice(schemeMatch[0].length).split(/[/?#]/, 1)[0];
  if (!authority) return null;
  try {
    const url = new URL(value);
    if (url.protocol !== 'https:' && url.protocol !== 'http:') return null;
    return url.host ? url.href : null;
  } catch {
    return null;
  }
}

export async function handleExternalURL(raw: string): Promise<boolean> {
  const safeURL = safeExternalURL(raw);
  if (!safeURL) return false;
  if (!canUseHostOpenExternalURL()) {
    window.open(safeURL, '_blank', 'noopener,noreferrer');
    return true;
  }
  try {
    await OpenExternalURL(safeURL);
    return true;
  } catch (err) {
    addToast('error', `Failed to open browser: ${errString(err)}`);
    return true;
  }
}

export function canUseHostOpenExternalURL(hostname = currentHostname()): boolean {
  if (runMode() === 'client') return false;
  if (!hostname) return false;
  return isLoopbackHostname(hostname);
}

export function installExternalLinkDelegate(): () => void {
  if (typeof document === 'undefined') return () => {};
  if (delegateInstallCount === 0) {
    document.addEventListener('click', handleExternalLinkClick);
    document.addEventListener('auxclick', handleExternalLinkClick);
  }
  delegateInstallCount += 1;
  return () => {
    delegateInstallCount = Math.max(0, delegateInstallCount - 1);
    if (delegateInstallCount === 0) {
      document.removeEventListener('click', handleExternalLinkClick);
      document.removeEventListener('auxclick', handleExternalLinkClick);
    }
  };
}

function handleExternalLinkClick(event: MouseEvent): void {
  if (event.defaultPrevented) return;
  if (event.button !== 0 && event.button !== 1) return;

  const target = event.target;
  if (!(target instanceof Element)) return;

  const link = target.closest<HTMLAnchorElement>('a[href]');
  if (!link) return;
  if (link.classList.contains('editor-link')) return;

  const rawHref = link.getAttribute('href');
  const safeURL = safeExternalURL(rawHref);
  if (!safeURL) return;

  event.preventDefault();
  void handleExternalURL(safeURL);
}

function isLoopbackHostname(hostname: string): boolean {
  const host = hostname.toLowerCase();
  return host === 'localhost' || host === '::1' || host === '[::1]' || host.startsWith('127.');
}

function currentHostname(): string {
  if (typeof window === 'undefined') return '';
  return window.location.hostname;
}
