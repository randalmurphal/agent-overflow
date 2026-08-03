import { OpenExternalURL } from '../stores/bindings';
import { addToast } from '../stores/toast.svelte';
import { runMode } from '../transport/runMode';
import { errString } from './errors';
import { PATH_LINK_HREF_PREFIX } from './pathLinkExtension';

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

/**
 * Narrow a URL to the loopback dev-server subset. Backend triage already
 * classifies command output (internal/triage/dev_server_url.go), but meta
 * is untrusted data by the time it reaches a row, so the affordance
 * re-validates before offering to open anything. Wildcard bind addresses
 * are deliberately NOT accepted here — the backend rewrites 0.0.0.0 / ::
 * to localhost, and a browser cannot navigate to the raw form.
 */
export function loopbackDevServerURL(raw: string | null | undefined): string | null {
  const safeURL = safeExternalURL(raw);
  if (!safeURL) return null;
  try {
    return isLoopbackHostname(new URL(safeURL).hostname) ? safeURL : null;
  } catch {
    return null;
  }
}

/** Compact `host:port` label for a dev-server affordance. */
export function devServerLabel(url: string): string {
  try {
    return new URL(url).host;
  } catch {
    return url;
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

/**
 * Resolve the external URL an event landed on, or null when the target is
 * not an outbound link. Shared by the click delegate and the right-click
 * menu host so both agree on what counts as external: path links
 * (`agent-overflow:open?path=…`) are an editor affordance, not a URL, and
 * anything that is not http(s) with a host is left to the browser.
 */
export function externalURLForEventTarget(target: EventTarget | null): string | null {
  if (!(target instanceof Element)) return null;
  const link = target.closest<HTMLAnchorElement>('a[href]');
  if (!link) return null;
  const rawHref = link.getAttribute('href');
  if (rawHref && rawHref.startsWith(PATH_LINK_HREF_PREFIX)) return null;
  return safeExternalURL(rawHref);
}

function handleExternalLinkClick(event: MouseEvent): void {
  if (event.defaultPrevented) return;
  if (event.button !== 0 && event.button !== 1) return;

  const safeURL = externalURLForEventTarget(event.target);
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
