// What a person reads when the computer a page called refused the call
// before running it: it does not have the call, or could not read its
// arguments (transport/dispatchRefusal.ts). Every sentence for it is in
// `versionMismatchSentence`; the transport carries the nameless default and
// asks the wording installed here when the refusal's message is first read,
// so no surface words it again.
//
// The facts are ones the page already holds in memory. The refusing
// computer's name and release (its hello's `bundleVersion`, which a
// computer before v0.0.15, or one that cannot build its bundle, does not
// send) are captured when its refusal arrives, so a reconnect or detach
// before the message is read does not change what it says. Read when the
// message is built: the page's own release (PAGE_VERSION), what the phone's
// bundle updater has confirmed (stores/bundleNotice.svelte.ts), and where
// this page's code came from, which decides the remedy:
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
import {
  installDispatchRefusalWording,
  type DispatchRefusalKind,
  type RefusingComputer,
} from '../transport/dispatchRefusal';
import { hasHomeEndpoint } from '../transport/homeEndpoint';
import { isWebviewHosted } from '../transport/pageHost';
import { isClientMode } from '../transport/runMode';
import { attachedBackendNameNow } from './attachedBackends.svelte';
import { bundleUpdate, type BundleUpdate } from './bundleNotice.svelte';
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
  /** For `device`: what the bundle updater has confirmed about the next
   * release (stores/bundleNotice.svelte.ts). */
  update: BundleUpdate;
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
    return `${lead} ${computer} runs the same version of Agent Overflow as ${here} (${pageVersion}). Report this as a bug in Agent Overflow.`;
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
    case 'device': {
      const { update } = facts;
      const enough = update.kind !== 'none' && (compareBundleVersions(update.version, computerVersion) ?? -1) >= 0;
      if (enough && update.kind === 'ready') return `${what} Close and reopen the app to load ${update.version}.`;
      if (enough) return `${what} Agent Overflow ${update.version} is downloading. When the app says it is ready, close and reopen it.`;
      return `${what} Install a newer version of the Agent Overflow app on this device.`;
    }
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
 * The refusing computer's name and release when its refusal arrived: one
 * name, by key, from memory.
 */
export function captureRefusingComputer(backend: BackendKey): RefusingComputer {
  return { backend, name: attachedBackendNameNow(backend), version: getTransportHelloFor(backend)?.bundleVersion ?? '' };
}

/**
 * The sentence for a refusal of `kind` by `computer`, or null when that
 * computer was no longer attached, which leaves the transport's nameless
 * sentence. What it was is as captured; what the person can do now (which
 * computer served this page, what the phone has staged) is read now, and
 * the name of the computer that served the page only when the remedy
 * needs it.
 */
export function versionMismatchMessage(kind: DispatchRefusalKind, computer: RefusingComputer): string | null {
  if (computer.name === null) return null;
  const source = pageSource();
  return versionMismatchSentence({
    kind,
    computer: computer.name,
    computerVersion: computer.version,
    computerServedPage: source === 'served' && computer.backend === HOME_BACKEND,
    pageVersion: PAGE_VERSION,
    page: isNativeShell() ? 'app' : isWebviewHosted() ? 'window' : 'page',
    source,
    server: () => ({
      name: attachedBackendNameNow(HOME_BACKEND) ?? 'the computer that served this page',
      version: getTransportHelloFor(HOME_BACKEND)?.bundleVersion ?? '',
    }),
    update: source === 'device' ? bundleUpdate() : { kind: 'none' },
  });
}

/** Called once at boot by `src/main.ts`. */
export function installVersionMismatchWording(): void {
  installDispatchRefusalWording({ capture: captureRefusingComputer, word: versionMismatchMessage });
}
