// The dev servers one machine is willing to share, and the preview URLs
// this page can open onto them (docs/specs/remote-access.md §7, the port
// gateway).
//
// One entry PER BACKEND, for the reason the update store beside it is per
// backend: `localhost:5173` names a different listener on every machine,
// and a page attached to two of them has two answers. Keyed by registry
// id; a frame's origin is translated once through `backendKeyForOrigin`.
//
// Three things this store decides, and nothing else decides them:
//
//   - Whether a `localhost:<port>` link on a thread whose machine is not
//     this page's is reachable at all. `previewFor` answers `open`,
//     `not-shared` or `no-address`, and the markdown rewrite and the
//     command row's dev-server chip both read that one answer.
//   - Whether the rewrite is ARMED for a machine at all. `previewSignature`
//     is empty until that machine has sent a list, and a caller that has no
//     signature leaves links exactly as they were: an inert "not shared"
//     link rendered before the first frame would be a wrong sentence, not
//     a slow one.
//   - What a click opens. `MintPreviewURL` answers an absolute URL carrying
//     a single-use ticket, and it is opened through the one external-open
//     wrapper — never assembled here.
//
// The list is execute-tier (`preview:open`): it names the ports a machine
// is listening on, so a view-only device does not get to read it. The
// allow / disallow pair is `access:admin`, because it edits that machine's
// `network.previewPorts`.

import {
  AllowPreviewPort,
  DisallowPreviewPort,
  GetDevServers,
  MintPreviewURL,
  type DevServer,
  type DevServerList,
} from './bindings';
import { wailsEventOn } from './wailsEvents';
import { createKeyedSignalRegistry } from './keyedSignalRegistry.svelte';
import { threadMachine } from './attachedBackends.svelte';
import { isMethodUnavailableError, onBackendHelloChange } from './transportStatus.svelte';
import {
  attachedBackends as registryBackends,
  backendKeyForOrigin,
  withBackendTarget,
} from '../transport/backends';
import type { BackendKey } from '../transport/backendKey';
import { hasScope } from '../transport/scopes';
import { isScopeRefusal } from '../transport/scopeRefusal';
import { handleExternalURL, installPreviewLinkActions } from '../utils/externalLinks';
import { userFacingError } from '../utils/userFacingError';

export type { DevServer, DevServerList };

/** Everything this page knows about one machine's shareable ports. */
export interface MachineDevServers {
  /** The last list read or pushed; null until the first frame lands. */
  list: DevServerList | null;
  /** A failed read that was not a refusal. Refusals leave `list` null and say nothing. */
  loadError: string;
  /** A read is in flight. */
  loading: boolean;
  /** A failed allow / disallow, as a sentence for the person who pressed it. */
  actionError: string;
  /**
   * Changes exactly when this machine's preview answer changes, and is
   * EMPTY until a list has landed. A memoised render context folds it in
   * to decide whether to rebuild its markdown tree, so it must move on a
   * new shared port and stand still on a frame that says the same thing.
   */
  signature: string;
}

const EMPTY: MachineDevServers = Object.freeze({
  list: null,
  loadError: '',
  loading: false,
  actionError: '',
  signature: '',
});

const machines = createKeyedSignalRegistry<MachineDevServers>(EMPTY);

/**
 * What a page has to re-render for. The preview host, plus the ports that
 * are shareable on it, in list order — the rest of a `DevServer` row (pid,
 * process name, the owning thread) changes on every discovery tick and
 * changes no link.
 */
function signatureOf(list: DevServerList): string {
  let ports = '';
  for (const server of list.servers ?? []) {
    if (server.allowed) ports += `${server.port},`;
  }
  return `${list.previewHost ?? ''}|${ports}`;
}

function patch(key: BackendKey, changes: Partial<MachineDevServers>): void {
  machines.set(key, { ...machines.get(key), ...changes });
}

function applyList(key: BackendKey, list: DevServerList): void {
  patch(key, { list, signature: signatureOf(list), loadError: '', loading: false });
}

/** One machine's dev-server state. Reactive on that machine's box alone. */
export function machineDevServers(key: BackendKey): MachineDevServers {
  return machines.get(key);
}

/**
 * The ports this machine is sharing, in list order. What Settings → Network
 * renders a row per; empty where nothing has been allowed.
 */
export function allowedPreviewPorts(key: BackendKey): readonly number[] {
  const list = machines.get(key).list;
  if (!list) return [];
  const ports: number[] = [];
  for (const server of list.servers ?? []) {
    if (server.allowed) ports.push(server.port);
  }
  return ports;
}

/**
 * Whether a rewrite may run against this machine at all. Empty until it has
 * answered once, which is what keeps a link plain rather than wrongly inert
 * on the way to the first frame.
 */
export function previewSignature(key: BackendKey): string {
  return machines.get(key).signature;
}

/** Why a preview link is live, dead, or has nowhere to point. */
export type PreviewAvailability =
  | { kind: 'open' }
  | { kind: 'not-shared' }
  | { kind: 'no-address' };

const PREVIEW_OPEN: PreviewAvailability = Object.freeze({ kind: 'open' });
const PREVIEW_NOT_SHARED: PreviewAvailability = Object.freeze({ kind: 'not-shared' });
const PREVIEW_NO_ADDRESS: PreviewAvailability = Object.freeze({ kind: 'no-address' });

/**
 * Can this page reach `port` on that machine?
 *
 * `no-address` wins over `not-shared`: a machine with no tailnet and no LAN
 * address has nowhere to serve ANY preview, so telling somebody to allow a
 * port there would send them to a control that changes nothing.
 */
export function previewFor(key: BackendKey, port: number): PreviewAvailability {
  const list = machines.get(key).list;
  if (!list) return PREVIEW_NOT_SHARED;
  if ((list.previewHost ?? '') === '') return PREVIEW_NO_ADDRESS;
  for (const server of list.servers ?? []) {
    if (server.port === port && server.allowed) return PREVIEW_OPEN;
  }
  return PREVIEW_NOT_SHARED;
}

/**
 * Read one machine's list. A PASSIVE load, so it asks before it fires
 * (stores/AGENTS.md): a session without `preview:open` on that backend
 * issues nothing. A backend older than this bundle refuses the call by
 * name, and that is the same silence as a machine with no dev servers.
 */
export async function loadDevServers(key: BackendKey): Promise<void> {
  if (!hasScope('preview:open', key)) return;
  if (machines.get(key).loading) return;
  patch(key, { loading: true });
  try {
    const list = await withBackendTarget(key, () => GetDevServers());
    applyList(key, list);
  } catch (err) {
    patch(key, { loading: false });
    if (isScopeRefusal(err) || isMethodUnavailableError(err)) return;
    patch(key, { loadError: userFacingError(err, 'Could not read the dev servers.') });
  }
}

/**
 * Add a port to that machine's preview set. The link goes live on the next
 * list frame, which the backend pushes — nothing is applied optimistically,
 * because the machine decides whether it can open a listener there at all.
 */
export async function allowPreviewPort(key: BackendKey, port: number): Promise<void> {
  await changePreviewPort(key, port, true);
}

/** Take a port back out of that machine's preview set. */
export async function disallowPreviewPort(key: BackendKey, port: number): Promise<void> {
  await changePreviewPort(key, port, false);
}

async function changePreviewPort(key: BackendKey, port: number, allow: boolean): Promise<void> {
  if (!hasScope('access:admin', key)) return;
  if (!Number.isSafeInteger(port) || port < 1 || port > 65535) return;
  patch(key, { actionError: '' });
  try {
    await withBackendTarget(key, () =>
      allow ? AllowPreviewPort(port) : DisallowPreviewPort(port),
    );
  } catch (err) {
    patch(key, {
      actionError: userFacingError(
        err,
        allow ? 'Could not share that port.' : 'Could not stop sharing that port.',
      ),
    });
  }
}

/**
 * Open the preview for a port on the thread's machine.
 *
 * The URL is MINTED rather than assembled: it carries a 60-second
 * single-use ticket the preview listener consumes on the first hit, so a
 * URL this page built itself would be a page that never loads. Opening
 * goes through the one external-open wrapper, which is what routes it to
 * the host binding or to the browser.
 */
export async function openPreview(threadId: string, port: number, path: string): Promise<void> {
  const key = threadMachine(threadId, null);
  try {
    const url = await withBackendTarget(key, () => MintPreviewURL(threadId, port, path));
    if (url) await handleExternalURL(url);
  } catch (err) {
    patch(key, { actionError: userFacingError(err, 'Could not open that preview.') });
  }
}

let cancel: (() => void) | null = null;

/**
 * Subscribe to `devserver:list` and read every attached machine's list on
 * its hello, now and on every reconnect. Also installs the two actions the
 * external-link delegate calls, which is why the delegate takes them by
 * REGISTRATION rather than by import: `utils/externalLinks.ts` is what this
 * module opens a minted URL through, and an import back would close a ring.
 *
 * Idempotent; answers a teardown.
 */
export function initDevServers(): () => void {
  if (cancel !== null) return stopDevServers;
  installPreviewLinkActions({ open: openPreview, allow: allowPreviewPort });
  const cancels = [
    wailsEventOn<DevServerList>('devserver:list', (list, origin) => {
      applyList(backendKeyForOrigin(origin.backendId), list);
    }),
    onBackendHelloChange((key, hello) => {
      if (hello !== null) {
        void loadDevServers(key);
        return;
      }
      // A null hello is a dropped socket OR a detached backend. Only the
      // second forgets: a machine whose socket is re-dialing still has the
      // same dev servers, and blanking the list would turn every live
      // preview link inert for the length of the outage.
      if (!registryBackends().some((b) => b.id === key)) machines.drop(key);
    }),
  ];
  cancel = () => {
    for (const c of cancels) c();
  };
  return stopDevServers;
}

export function stopDevServers(): void {
  cancel?.();
  cancel = null;
  installPreviewLinkActions(null);
}

/** Test-only: drop every machine and the subscriptions. */
export function resetDevServersForTest(): void {
  stopDevServers();
  machines.reset();
}
