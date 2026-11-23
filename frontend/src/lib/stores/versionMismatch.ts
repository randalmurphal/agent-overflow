// What a person reads when the computer a page called refused the call
// before running it: it does not have the call, or could not read its
// arguments (transport/dispatchRefusal.ts). Every sentence for it is in
// `versionMismatchSentence`; the transport carries the nameless default and
// asks the wording installed here when the refusal's message is first read,
// so no surface words it again.
//
// The facts are ones the page already holds in memory: its own release
// (PAGE_VERSION), each computer's release from its hello (`bundleVersion`,
// which a computer before v0.0.15, or one that cannot build its bundle,
// does not send), the computer's name, the release the phone app has
// staged for its next start (stores/bundleNotice.svelte.ts), and where this
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
// A sentence says the versions differ only when both are known and differ,
// and gives a direction only from the comparison that proves it.

import { compareBundleVersions } from '../native/bundleVersion';
import { isNativeShell } from '../native/platform';
import { PAGE_VERSION } from '../pageVersion';
import { HOME_BACKEND, type BackendKey } from '../transport/backendKey';
import { installDispatchRefusalWording, type DispatchRefusalKind } from '../transport/dispatchRefusal';
import { hasHomeEndpoint } from '../transport/homeEndpoint';
import { isWebviewHosted } from '../transport/pageHost';
import { isClientMode } from '../transport/runMode';
import { attachedBackendNameNow } from './attachedBackends.svelte';
import { stagedBundleVersion } from './bundleNotice.svelte';
import { getTransportHelloFor } from './transportStatus.svelte';

export type PageSource = 'served' | 'local' | 'device';

export interface VersionMismatchFacts {
  kind: DispatchRefusalKind;
  /** The refusing computer's name. */
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
  /** For `served` when another computer served this page: its name and
   * its release now, or '' when it sent none. Read only when needed. */
  server: () => { name: string; version: string };
  /** For `device`: the release the app loads at its next start, or ''. */
  stagedVersion: string;
}

const PAGE_NOUN = { window: 'this window', page: 'this page', app: 'this app' } as const;

/** The one table of sentences. Pure; never throws. */
export function versionMismatchSentence(facts: VersionMismatchFacts): string {
  const { computer, computerVersion, pageVersion } = facts;
  const lead = facts.kind === 'unsupported'
    ? `${computer} does not support this action.`
    : `${computer} could not read this request.`;
  const here = PAGE_NOUN[facts.page];
  const reload = facts.page === 'window' ? 'reload the window' : 'reload the page';
  const order = compareBundleVersions(computerVersion, pageVersion);
  if (order === null) {
    const check = `This ${facts.page} cannot tell which version of Agent Overflow ${computer} runs. Check that ${computer} and ${here} run the same version, and update whichever is older.`;
    return facts.computerServedPage
      ? `${lead} ${check} If ${computer} was updated while ${here} was open, ${reload}.`
      : `${lead} ${check}`;
  }
  if (order === 0) {
    return `${lead} ${computer} runs the same version of Agent Overflow as ${here} (${pageVersion}). Copy the details of this error and report it.`;
  }
  const now = facts.computerServedPage ? 'now runs' : 'runs';
  const relation = order < 0 ? 'older' : 'newer';
  const what = `${lead} ${computer} ${now} Agent Overflow ${computerVersion}, ${relation} than ${here} (${pageVersion}).`;
  if (facts.computerServedPage) return `${what} ${capitalize(reload)}.`;
  if (order < 0) return `${what} Update Agent Overflow on ${computer}.`;
  switch (facts.source) {
    case 'served': {
      const server = facts.server();
      const serverOrder = compareBundleVersions(server.version, computerVersion);
      if (serverOrder === null) {
        return `${what} If ${server.name} runs ${computerVersion} or newer, ${reload}. If not, update Agent Overflow on ${server.name}, then ${reload}.`;
      }
      return serverOrder >= 0
        ? `${what} ${capitalize(reload)}.`
        : `${what} Update Agent Overflow on ${server.name}, then ${reload}.`;
    }
    case 'local':
      return `${what} Update Agent Overflow on this computer.`;
    case 'device':
      return (compareBundleVersions(facts.stagedVersion, computerVersion) ?? -1) >= 0
        ? `${what} Close and reopen the app to load ${facts.stagedVersion}.`
        : `${what} Install a newer version of the Agent Overflow app on this device.`;
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

/**
 * The sentence for a refusal of `kind` from `backend`, or null when that
 * computer has detached, which leaves the transport's nameless sentence.
 * Looks up at most two names, the refusing computer's and, only when the
 * remedy needs it, the one that served this page, by key from memory.
 */
export function versionMismatchMessage(kind: DispatchRefusalKind, backend: BackendKey): string | null {
  const computer = attachedBackendNameNow(backend);
  if (computer === null) return null;
  const source = pageSource();
  return versionMismatchSentence({
    kind,
    computer,
    computerVersion: getTransportHelloFor(backend)?.bundleVersion ?? '',
    computerServedPage: source === 'served' && backend === HOME_BACKEND,
    pageVersion: PAGE_VERSION,
    page: isNativeShell() ? 'app' : isWebviewHosted() ? 'window' : 'page',
    source,
    server: () => ({
      name: attachedBackendNameNow(HOME_BACKEND) ?? 'the computer that served this page',
      version: getTransportHelloFor(HOME_BACKEND)?.bundleVersion ?? '',
    }),
    stagedVersion: source === 'device' ? stagedBundleVersion() : '',
  });
}

/** Called once at boot by `src/main.ts`. */
export function installVersionMismatchWording(): void {
  installDispatchRefusalWording(versionMismatchMessage);
}
