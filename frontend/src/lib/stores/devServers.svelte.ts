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

import { untrack } from 'svelte';
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
import {
  attachedBackendEntry,
  backendDisplayName,
  threadActsHere,
  threadMachine,
} from './attachedBackends.svelte';
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
import { parsePreviewTarget } from '../utils/previewLinkExtension';
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

/**
 * Fold changes into one machine's box.
 *
 * The read of the current value is UNTRACKED, and every mutator below goes
 * through here for that reason. A passive load is called from a mounted
 * surface, so a tracked read-then-write would make that surface a dependent
 * of the box the write moves, and it would run again, and load again. The
 * subscriber the writes exist for reads through the accessors instead, which
 * track normally.
 */
function patch(key: BackendKey, changes: Partial<MachineDevServers>): void {
  machines.set(key, { ...untrack(() => machines.get(key)), ...changes });
}

function listChanges(list: DevServerList): Partial<MachineDevServers> {
  return { list, signature: signatureOf(list), loadError: '' };
}

/**
 * Count of PUSHED frames applied to one machine, which a read in flight
 * snapshots so it can tell whether its answer is still the newest thing
 * known about that machine. Deliberately not a field on the box: it is a
 * concurrency detail, and a render has no reason to wake for it.
 */
const framesApplied = new Map<BackendKey, number>();

function framesSeen(key: BackendKey): number {
  return framesApplied.get(key) ?? 0;
}

/** Apply a pushed `devserver:list` frame. */
function applyFrame(key: BackendKey, list: DevServerList): void {
  framesApplied.set(key, framesSeen(key) + 1);
  patch(key, listChanges(list));
}

/** One machine's dev-server state. Reactive on that machine's box alone. */
export function machineDevServers(key: BackendKey): MachineDevServers {
  return machines.get(key);
}

/**
 * Every port this machine is sharing, in list order, however it came to be
 * shared. What the Settings field checks a typed port against, so that
 * re-adding one already reachable is refused rather than called to no
 * effect.
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
 * The ports in this machine's PERSISTED set, in list order. The only ones
 * `DisallowPreviewPort` can act on, so the only ones Settings offers to
 * stop sharing. A port named by hand is always `allowed`, whatever else is
 * true of it (`internal/devservers`), which is what makes the source the
 * whole test.
 */
export function sharedPreviewPorts(key: BackendKey): readonly number[] {
  const list = machines.get(key).list;
  if (!list) return [];
  const ports: number[] = [];
  for (const server of list.servers ?? []) {
    if (server.allowed && server.source === 'allowed') ports.push(server.port);
  }
  return ports;
}

/** A port reachable because a thread on this machine is running a server there. */
export interface AttributedPreviewPort {
  port: number;
  /** What is running it, empty when the machine did not name a process. */
  process: string;
}

/**
 * The ports shared for as long as a thread keeps them alive, in list order.
 * Settings shows them so the set it lists is the set that is reachable, but
 * offers no control: taking one back means ending the run that owns it, and
 * `DisallowPreviewPort` only edits the persisted set.
 */
export function attributedPreviewPorts(key: BackendKey): readonly AttributedPreviewPort[] {
  const list = machines.get(key).list;
  if (!list) return [];
  const rows: AttributedPreviewPort[] = [];
  for (const server of list.servers ?? []) {
    if (server.allowed && server.source === 'attributed') {
      rows.push({ port: server.port, process: server.process ?? '' });
    }
  }
  return rows;
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
  return availability((list.previewHost ?? '') !== '', portIsAllowed(list, port));
}

function availability(hasAddress: boolean, allowed: boolean): PreviewAvailability {
  if (!hasAddress) return PREVIEW_NO_ADDRESS;
  return allowed ? PREVIEW_OPEN : PREVIEW_NOT_SHARED;
}

function portIsAllowed(list: DevServerList, port: number): boolean {
  for (const server of list.servers ?? []) {
    if (server.port === port && server.allowed) return true;
  }
  return false;
}

/**
 * Whether that machine can see something answering on `port` right now.
 *
 * Off the owner's own screen this replaces the loopback probe entirely: the
 * machine is the only party that can reach its own `localhost`, and its list
 * already says which of its ports have a listener.
 */
export function devServerListening(key: BackendKey, port: number): boolean {
  for (const server of machines.get(key).list?.servers ?? []) {
    if (server.port === port) return server.listening;
  }
  return false;
}

/**
 * Whether reaching a thread's `localhost` ports has to go through the port
 * gateway rather than straight out of this page.
 *
 * False in exactly one case, the ordinary desktop one: the thread runs on the
 * page's own machine AND this session can act there, so `localhost` already
 * means what it says. That is `threadActsHere`, and this is preview's name
 * for its negation: the click delegate asks the same question about the
 * companion browser, so neither spells the test out a second time.
 */
export function previewRouted(threadId: string): boolean {
  return !threadActsHere(threadId);
}

// ---------------------------------------------------------------------------
// What the markdown rewrite reads
// ---------------------------------------------------------------------------

/**
 * The answer the markdown link rewrite needs, resolved ONCE per change and
 * then read without touching this store again.
 *
 * `resolve` closes over a snapshot rather than reading the registry, and
 * that is deliberate: it is called from inside marked's tokenizer, during a
 * render, and a reactive read there would make every markdown tree in the
 * timeline a dependent of a list frame that concerns one thread.
 */
export interface PreviewLinkTarget {
  /** The thread whose machine the ports belong to. */
  threadId: string;
  /** That machine. */
  backend: BackendKey;
  /** What the reader calls it. */
  machine: string;
  /** Whether this session may offer to share a port that is not shared. */
  canAllow: boolean;
  /** Changes exactly when a rewritten link would render differently. */
  key: string;
  resolve(port: number): PreviewAvailability;
}

/**
 * Whether a thread's `localhost` links are the reader's own, and the state
 * they resolve against when they are not.
 *
 * Null means leave the markdown alone, for one of three reasons: the surface
 * names no thread, the machine has not answered yet (`signature` empty), or
 * the thread runs on the page's own machine AND this session can act there,
 * which is the ordinary desktop case where `localhost` already means what it
 * says.
 */
export function previewLinkTargetFor(threadId: string): PreviewLinkTarget | null {
  if (threadId === '' || !previewRouted(threadId)) return null;
  const backend = threadMachine(threadId, null);
  const machineState = machines.get(backend);
  if (machineState.signature === '') return null;
  const list = machineState.list;
  const hasAddress = (list?.previewHost ?? '') !== '';
  const allowed = new Set<number>();
  for (const server of list?.servers ?? []) {
    if (server.allowed) allowed.add(server.port);
  }
  const entry = attachedBackendEntry(backend);
  const machine = entry ? backendDisplayName(entry) : 'that machine';
  const canAllow = hasScope('access:admin', backend);
  return {
    threadId,
    backend,
    machine,
    canAllow,
    key: previewRewriteKeyFrom(threadId, backend, machine, canAllow, machineState.signature),
    resolve: (port) => availability(hasAddress, allowed.has(port)),
  };
}

/**
 * The same decision as `previewLinkTargetFor`, as a plain string and without
 * building anything. A memoised render context folds this in to decide
 * whether a markdown tree has to be rebuilt, so it is called far more often
 * than the rewrite is; empty means the rewrite is off.
 */
export function previewRewriteKey(threadId: string): string {
  if (threadId === '' || !previewRouted(threadId)) return '';
  const backend = threadMachine(threadId, null);
  const signature = machines.get(backend).signature;
  if (signature === '') return '';
  const entry = attachedBackendEntry(backend);
  return previewRewriteKeyFrom(
    threadId,
    backend,
    entry ? backendDisplayName(entry) : 'that machine',
    hasScope('access:admin', backend),
    signature,
  );
}

function previewRewriteKeyFrom(
  threadId: string,
  backend: BackendKey,
  machine: string,
  canAllow: boolean,
  signature: string,
): string {
  return `${threadId}|${backend}|${machine}|${canAllow ? '1' : '0'}|${signature}`;
}

/**
 * Read one machine's list. A PASSIVE load, so it asks before it fires
 * (stores/AGENTS.md): a session without `preview:open` on that backend
 * issues nothing. A backend older than this bundle refuses the call by
 * name, and that is the same silence as a machine with no dev servers.
 */
export async function loadDevServers(key: BackendKey): Promise<void> {
  if (!hasScope('preview:open', key)) return;
  if (untrack(() => machines.get(key).loading)) return;
  // The machine pushes frames on its own clock, so one can land while this
  // read is in flight — and it is the newer of the two. Dropping the stale
  // answer costs a comparison; keeping it would show a list the machine has
  // already moved on from until the next tick corrected it.
  const seen = framesSeen(key);
  patch(key, { loading: true });
  try {
    const list = await withBackendTarget(key, () => GetDevServers());
    patch(key, framesSeen(key) === seen ? { ...listChanges(list), loading: false } : { loading: false });
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

/** A live, reachable dev server on another machine, ready to be offered. */
export interface PreviewChip {
  /** The URL the command announced, kept for the label and the title. */
  url: string;
  threadId: string;
  port: number;
  path: string;
  /** What the reader calls the machine it is on. */
  machine: string;
}

/**
 * The dev-server chip for a command row, when the row's thread is not on the
 * machine reading it. Null means the row falls back to the loopback probe,
 * which is the only thing that works when it IS.
 *
 * Three ways to be null besides that: the URL is not a loopback dev server,
 * the machine sees nothing answering on the port, or the port is not shared.
 * The chip is an affordance this app adds, so it appears only when pressing
 * it lands somewhere; the states that go nowhere are said by the link in the
 * prose that named the port, not by a dead button.
 */
export function previewChipFor(threadId: string, url: string): PreviewChip | null {
  if (threadId === '' || url === '' || !previewRouted(threadId)) return null;
  const parsed = parsePreviewTarget(url);
  if (!parsed) return null;
  const backend = threadMachine(threadId, null);
  if (!devServerListening(backend, parsed.port)) return null;
  if (previewFor(backend, parsed.port).kind !== 'open') return null;
  const entry = attachedBackendEntry(backend);
  return {
    url,
    threadId,
    port: parsed.port,
    path: parsed.path,
    machine: entry ? backendDisplayName(entry) : 'that machine',
  };
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
      applyFrame(backendKeyForOrigin(origin.backendId), list);
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
      if (registryBackends().some((b) => b.id === key)) return;
      machines.drop(key);
      framesApplied.delete(key);
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
  framesApplied.clear();
}
