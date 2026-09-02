import { OpenExternalURL } from '../stores/bindings';
import { addToast } from '../stores/toast.svelte';
import { runMode } from '../transport/runMode';
import type { BackendKey } from '../transport/backendKey';
import { errString } from './errors';
import { PATH_LINK_HREF_PREFIX } from './pathLinkExtension';

let delegateInstallCount = 0;

/**
 * What a preview anchor's click does, supplied by `stores/devServers`.
 *
 * Taken by REGISTRATION rather than by import, and for the reason the
 * pane registry arms its focused-pane resolver the same way: that store
 * opens a minted URL through `handleExternalURL` below, so an import back
 * from here would close a ring around two modules that both run at boot.
 * The store installs these in `initDevServers()` and clears them on
 * teardown; with none installed there are no preview anchors in the
 * document either, because the rewrite is armed by the same list frame.
 */
export interface PreviewLinkActions {
  open: (threadId: string, port: number, path: string) => Promise<void>;
  allow: (backend: BackendKey, port: number) => Promise<void>;
}

let previewActions: PreviewLinkActions | null = null;

export function installPreviewLinkActions(actions: PreviewLinkActions | null): void {
  previewActions = actions;
}

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

/**
 * The preview anchor a click landed on, or null. The anchor's `href` stays
 * the ORIGINAL `localhost:<port>` URL so copying and inspecting it say what
 * the agent said; where it goes is decided here, from the data attributes
 * the markdown rewrite stamped (`utils/previewLinkExtension.ts`).
 */
function previewLinkForEventTarget(target: EventTarget | null): HTMLElement | null {
  if (!(target instanceof Element)) return null;
  return target.closest<HTMLElement>('[data-preview-port]');
}

function handleExternalLinkClick(event: MouseEvent): void {
  if (event.defaultPrevented) return;
  if (event.button !== 0 && event.button !== 1) return;

  // The inline Allow port action beside a link that is not shared. It is a
  // real button rather than an anchor, so it is checked before the link
  // resolution below ever runs.
  if (event.button === 0 && event.target instanceof Element) {
    const allow = event.target.closest<HTMLElement>('[data-preview-allow]');
    if (allow) {
      event.preventDefault();
      const port = Number(allow.dataset.previewAllow ?? '');
      if (previewActions && Number.isSafeInteger(port)) {
        void previewActions.allow(allow.dataset.previewBackend ?? '', port);
      }
      return;
    }
  }

  const preview = previewLinkForEventTarget(event.target);
  if (preview) {
    // Every state swallows the click: the href names a listener on the
    // machine the agent is on, and following it here would load whatever
    // answers on that port of the machine the READER is on, or nothing.
    event.preventDefault();
    if (event.button !== 0) return;
    if ((preview.dataset.previewState ?? '') !== 'open') return;
    const port = Number(preview.dataset.previewPort ?? '');
    const threadId = preview.dataset.previewThread ?? '';
    if (!previewActions || !threadId || !Number.isSafeInteger(port)) return;
    void previewActions.open(threadId, port, preview.dataset.previewPath ?? '/');
    return;
  }

  const safeURL = externalURLForEventTarget(event.target);
  if (!safeURL) return;

  event.preventDefault();
  void handleExternalURL(safeURL);
}

function isLoopbackHostname(hostname: string): boolean {
  const host = hostname.toLowerCase();
  if (host === 'localhost' || host === '::1' || host === '[::1]') return true;
  // A full dotted-quad match, not a `127.` prefix — `127.example.com`
  // is a resolvable public name, not a loopback address. Shorthand
  // forms (`127.1`) never reach here: WHATWG URL parsing normalizes
  // IPv4 hosts to the dotted quad before `.hostname` is read.
  return /^127\.\d{1,3}\.\d{1,3}\.\d{1,3}$/.test(host);
}

function currentHostname(): string {
  if (typeof window === 'undefined') return '';
  return window.location.hostname;
}
