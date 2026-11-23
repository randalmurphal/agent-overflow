// What a person reads when a computer refused a call it does not have
// (`method_not_found`): the page and that computer run different releases of
// Agent Overflow. Every sentence for it is in `versionMismatchSentence`; the
// transport carries the nameless default (transport/methodUnavailable.ts)
// and calls the wording installed here from every `TransportError` it builds
// for that code, so no surface words it again.
//
// The facts are ones the page already holds: its own release (PAGE_VERSION),
// each computer's release from its hello (`bundleVersion`, which a computer
// before v0.0.15 does not send), the computer's display name, and where this
// page's code came from, which decides the remedy:
//
//   served  the computer that answered this page's address served it (the
//           desktop window, a browser on a share link). Reloading loads that
//           computer's current release.
//   local   this computer's own installation served it (`--connect` and the
//           paired desktop frontend, transport/runMode.ts). Reloading loads
//           the same release again.
//   device  the phone app's own bundle, which updates from paired computers.
//
// Versions are compared as releases (native/bundleVersion.ts). A version
// missing, unparseable or equal on both sides says "a different version"
// rather than guess a direction.

import { compareBundleVersions } from '../native/bundleVersion';
import { isNativeShell } from '../native/platform';
import { PAGE_VERSION } from '../pageVersion';
import { HOME_BACKEND, type BackendKey } from '../transport/backendKey';
import { hasHomeEndpoint } from '../transport/homeEndpoint';
import { installMethodUnavailableWording } from '../transport/methodUnavailable';
import { isWebviewHosted } from '../transport/pageHost';
import { isClientMode } from '../transport/runMode';
import { attachedBackendEntry, backendDisplayName } from './attachedBackends.svelte';
import { getTransportHelloFor } from './transportStatus.svelte';

export type PageSource = 'served' | 'local' | 'device';

export interface VersionMismatchFacts {
  /** The refusing computer's display name. */
  computer: string;
  /** Its release from its hello, or '' when it sent none. */
  computerVersion: string;
  /** Whether the refusing computer is the one that served this page. */
  computerServedPage: boolean;
  /** This page's own release. */
  pageVersion: string;
  /** What the person calls what they are looking at. */
  page: 'window' | 'page' | 'app';
  source: PageSource;
  /** For `served`: the computer that served this page, and its release now. */
  server: string;
  serverVersion: string;
}

const PAGE_NOUN = { window: 'this window', page: 'this page', app: 'this app' } as const;

/** The one table of sentences. Pure; never throws. */
export function versionMismatchSentence(facts: VersionMismatchFacts): string {
  const order = compareBundleVersions(facts.computerVersion, facts.pageVersion) ?? 0;
  const relation = order > 0 ? 'a newer version' : order < 0 ? 'an older version' : 'a different version';
  const reload = facts.page === 'window' ? 'reload the window' : 'reload the page';
  const here = PAGE_NOUN[facts.page];
  if (facts.computerServedPage) {
    return `${facts.computer} now runs ${relation} of Agent Overflow than ${here}. ${capitalize(reload)}.`;
  }
  const what = `${facts.computer} runs ${relation} of Agent Overflow than ${here} and does not support this.`;
  if (order < 0) return `${what} Update Agent Overflow on ${facts.computer}.`;
  if (order === 0) return `${what} Update Agent Overflow so both run the same version.`;
  switch (facts.source) {
    case 'served':
      return (compareBundleVersions(facts.serverVersion, facts.pageVersion) ?? 0) > 0
        ? `${what} ${capitalize(reload)}.`
        : `${what} Update Agent Overflow on ${facts.server}, then ${reload}.`;
    case 'local':
      return `${what} Update Agent Overflow on this computer.`;
    case 'device':
      return `${what} Update Agent Overflow on this device.`;
  }
}

function capitalize(text: string): string {
  return text.charAt(0).toUpperCase() + text.slice(1);
}

function pageSource(): PageSource {
  if (isNativeShell() || hasHomeEndpoint()) return 'device';
  if (isClientMode()) return 'local';
  return 'served';
}

function displayName(backend: BackendKey): string | null {
  const entry = attachedBackendEntry(backend);
  return entry === undefined ? null : backendDisplayName(entry);
}

/**
 * The sentence for a `method_not_found` from `backend`, or null when that
 * computer, or the one that served this page, is no longer attached and has
 * no name to give, which leaves the transport's nameless sentence.
 */
export function versionMismatchMessage(backend: BackendKey): string | null {
  const computer = displayName(backend);
  const source = pageSource();
  const server = source === 'served' ? displayName(HOME_BACKEND) : '';
  if (computer === null || server === null) return null;
  return versionMismatchSentence({
    computer,
    computerVersion: getTransportHelloFor(backend)?.bundleVersion ?? '',
    computerServedPage: source === 'served' && backend === HOME_BACKEND,
    pageVersion: PAGE_VERSION,
    page: isNativeShell() ? 'app' : isWebviewHosted() ? 'window' : 'page',
    source,
    server,
    serverVersion: getTransportHelloFor(HOME_BACKEND)?.bundleVersion ?? '',
  });
}

/** Called once at boot by `src/main.ts`. */
export function installVersionMismatchWording(): void {
  installMethodUnavailableWording(versionMismatchMessage);
}
