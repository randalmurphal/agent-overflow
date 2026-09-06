// Conversation handoffs belong to the hosts. This module holds only the
// bounded status projection and the currently edited frontend dialog.
import {
  BeginThreadTransfer, BindThreadTransferDestination, CreateThreadTransferOffer,
  GetThreadTransfers, GetThreadTransferIntent, RetryThreadTransfer,
  CancelThreadTransfer, DiscardUnpreparedThreadTransfer,
  GetThreadTransferDestinationProject, GetThread,
} from './bindings';
import type { Thread } from '../types/models';
import { type BackendKey } from '../transport/backendKey';
import { getBackendIdentity } from '../transport/backendIdentity';
import { backendById, withBackendTarget, attachedBackends } from '../transport/backends';
import { TransportError } from '../transport/wsClient';
import { backendReachable, threadMachine, getAttachedBackends } from './attachedBackends.svelte';
import { getTransportHelloFor, onBackendHelloChange } from './transportStatus.svelte';
import { hasScope } from '../transport/scopes';
import { projectBackend } from '../transport/entityIndex';
import { createKeyedSignalRegistry } from './keyedSignalRegistry.svelte';
import { wailsEventOn } from './wailsEvents';
import { userFacingError } from '../utils/userFacingError';
import { refreshSidebarProjections } from './eventsThreadRows';
import { randomId } from '../utils/randomId';

export type ConversationTransfer = Awaited<ReturnType<typeof GetThreadTransfers>>[number];
type TransferIntent = Awaited<ReturnType<typeof BeginThreadTransfer>>;
export type TransferKind = 'move' | 'copy';
export interface TransferRequest {
  operationID: string;
  thread: Thread;
  source: BackendKey;
  open: boolean;
  submitted: boolean;
  submitting: boolean;
  error: string;
  destination?: BackendKey;
  projectID?: string;
  kind?: TransferKind;
  includeWorkspace?: boolean;
  intent?: TransferIntent;
}

interface ComputerTransfers { rows: readonly ConversationTransfer[]; error: string }
const EMPTY: ComputerTransfers = Object.freeze({ rows: [], error: '' });
const computers = createKeyedSignalRegistry<ComputerTransfers>(EMPTY);
const revisions = new Map<BackendKey, number>();
let pending = $state.raw<TransferRequest | null>(null);

export function supportsConversationTransfer(backend: BackendKey): boolean {
  return getTransportHelloFor(backend)?.capabilities.includes('conversation.transfer.v1') ?? false;
}
export function computerTransfers(backend: BackendKey): ComputerTransfers { return computers.get(backend); }
export function pendingConversationTransfer(): TransferRequest | null { return pending; }
export function openConversationTransfer(thread: Thread, destination?: BackendKey): void {
  if (pending?.thread.id === thread.id && pending.submitted) {
    const previous = computers.get(pending.source).rows.find((row) => row.id === pending?.operationID);
    if (!previous || !terminal(previous)) { pending = { ...pending, open: true }; return; }
  }
  pending = { operationID: randomId(), thread, source: threadMachine(thread.id, thread.projectId), destination, open: true, submitted: false, submitting: false, error: '' };
}
export function canOfferConversationTransfer(thread: Thread): boolean {
  const source = threadMachine(thread.id, thread.projectId);
  return (thread.provider === 'claude' || thread.provider === 'codex') && !thread.parentThreadId && !thread.discussionId && !thread.mode?.startsWith('workflow') &&
    supportsConversationTransfer(source) && getAttachedBackends().some((entry) => entry.id !== source && supportsConversationTransfer(entry.id));
}
export function closeConversationTransfer(): void {
  if (pending) pending = { ...pending, open: false };
}
function patchRequest(id: string, patch: Partial<TransferRequest>): void {
  if (pending?.operationID === id) pending = { ...pending, ...patch };
}

export async function submitConversationTransfer(destination: BackendKey, projectID: string, kind: TransferKind, includeWorkspace: boolean): Promise<void> {
  const request = pending;
  if (!request || request.submitting) return;
  if (!hasScope('threads:operate', request.source) || !hasScope('threads:operate', destination) || (includeWorkspace && !hasScope('git:operate', destination))) {
    patchRequest(request.operationID, { error: 'Transfer access is needed on both computers, and Git access on the destination when including workspace changes.' }); return;
  }
  if (request.submitted && (request.destination !== destination || request.projectID !== projectID || request.kind !== kind || request.includeWorkspace !== includeWorkspace)) return;
  if (!supportsConversationTransfer(request.source) || !supportsConversationTransfer(destination)) {
    patchRequest(request.operationID, { error: 'Update both computers to use conversation transfers.' }); return;
  }
  if (!backendReachable(request.source) || !backendReachable(destination)) {
    patchRequest(request.operationID, { error: 'Connect both computers to start this transfer.' }); return;
  }
  if (projectBackend(projectID) !== destination || destination === request.source) {
    patchRequest(request.operationID, { error: 'Choose a project on another computer.' }); return;
  }
  const destinationID = getBackendIdentity(destination).backendId;
  const sourceID = getBackendIdentity(request.source).backendId;
  if (!destinationID || !sourceID) { patchRequest(request.operationID, { error: 'Waiting for the computers to identify themselves.' }); return; }
  patchRequest(request.operationID, { submitted: true, submitting: true, destination, projectID, kind, includeWorkspace, error: '' });
  let began = false;
  try {
    const intent = await withBackendTarget(request.source, () => BeginThreadTransfer(request.thread.id, request.operationID, destinationID, kind, includeWorkspace));
    began = true;
    if (intent.sourceBackendId !== sourceID || intent.destinationBackendId !== destinationID) throw new Error('A computer changed identity during transfer setup. Reconnect before retrying.');
    patchRequest(request.operationID, { intent });
    // The offer exists only in this call and in host-private recovery state.
    // Never place its grant in localStorage, diagnostic logs, or UI state.
    const offer = await withBackendTarget(destination, () => CreateThreadTransferOffer(intent, projectID, '', ''));
    const row = await withBackendTarget(request.source, () => BindThreadTransferDestination(request.thread.id, offer));
    receiveTransfer(request.source, row);
    void refreshComputerTransfers(destination);
  } catch (error) {
    // Begin returns an application refusal only before reserving this ID.
    // A first-attempt refusal may reopen the choices; a disconnect, internal
    // error or any retry remains uncertain and must keep the captured request.
    const declined = !request.submitted && !began && error instanceof TransportError &&
      ['method_error', 'bad_params', 'method_not_found', 'scope_required', 'auth_failed', 'thread_moved', 'thread_transfer_pending'].includes(error.code);
    patchRequest(request.operationID, { error: userFacingError(error), ...(declined ? { submitted: false } : {}) });
    void refreshComputerTransfers(request.source);
    void refreshComputerTransfers(destination);
  } finally { patchRequest(request.operationID, { submitting: false }); }
}

export async function recoverConversationTransfer(source: BackendKey, row: ConversationTransfer, thread?: Thread): Promise<void> {
  thread ??= await withBackendTarget(source, () => GetThread(row.threadId)) as Thread;
  const intent = await withBackendTarget(source, () => GetThreadTransferIntent(row.threadId, row.id));
  const destination = attachedBackends().find((entry) => entry.backendId === intent.destinationBackendId)?.id;
  if (!destination || !backendReachable(destination)) throw new Error('Reconnect the destination computer to finish setup.');
  const projectID = await withBackendTarget(destination, () => GetThreadTransferDestinationProject(row.id));
  pending = { operationID: row.id, thread, source, destination, projectID: projectID || undefined, kind: intent.kind as TransferKind, includeWorkspace: intent.includeWorkspace, intent, open: true, submitted: Boolean(projectID), submitting: false, error: '' };
}

export async function retryConversationTransfer(backend: BackendKey, row: ConversationTransfer): Promise<void> {
  await withBackendTarget(backend, () => RetryThreadTransfer(row.id));
  await refreshComputerTransfers(backend);
}
export async function cancelConversationTransfer(backend: BackendKey, row: ConversationTransfer): Promise<void> {
  if (row.direction === 'incoming') {
    await withBackendTarget(backend, () => DiscardUnpreparedThreadTransfer(row.id));
  } else {
    // An offer can exist on the destination even if its reply never reached
    // the frontend. Clearing an unprepared offer prevents an orphan blocking
    // a later move. A prepared recipient instead needs source cancellation.
    const peer = attachedBackends().find((entry) => entry.backendId === row.peerBackendId);
    if (peer && backendReachable(peer.id)) {
      try { await withBackendTarget(peer.id, () => DiscardUnpreparedThreadTransfer(row.id)); }
      catch { /* Source cancellation below carries the required proof. */ }
    }
    await withBackendTarget(backend, () => CancelThreadTransfer(row.threadId, row.id));
  }
  await refreshComputerTransfers(backend);
}

function receiveTransfer(backend: BackendKey, row: ConversationTransfer): void {
  const current = computers.get(backend);
  const previous = current.rows.find((value) => value.id === row.id);
  if (previous && previous.updatedAt > row.updatedAt) return;
  const rows = [row, ...current.rows.filter((value) => value.id !== row.id)]
    .sort((a, b) => Number(terminal(a)) - Number(terminal(b)) || b.updatedAt - a.updatedAt).slice(0, 100);
  computers.set(backend, { rows, error: '' });
  if (row.phase === 'complete' && previous?.phase !== 'complete') refreshSidebarProjections();
}
export function terminal(row: ConversationTransfer): boolean { return row.phase === 'complete' || row.phase === 'canceled'; }

export async function refreshComputerTransfers(backend: BackendKey): Promise<void> {
  if (!hasScope('threads:read', backend) || !supportsConversationTransfer(backend) || !backendReachable(backend)) return;
  const identity = getBackendIdentity(backend).backendId;
  const revision = (revisions.get(backend) ?? 0) + 1;
  revisions.set(backend, revision);
  const before = new Map(computers.get(backend).rows.map((row) => [row.id, row]));
  try {
    const rows = await withBackendTarget(backend, () => GetThreadTransfers());
    if (revisions.get(backend) !== revision || getBackendIdentity(backend).backendId !== identity) return;
    // Keep events delivered during this read, without discarding other rows
    // from the snapshot. A busy transfer must not hide a recovered operation.
    const merged = new Map(rows.map((row) => [row.id, row]));
    for (const row of computers.get(backend).rows) if (before.get(row.id) !== row) merged.set(row.id, row);
    computers.set(backend, { rows: [...merged.values()].sort((a, b) => Number(terminal(a)) - Number(terminal(b)) || b.updatedAt - a.updatedAt).slice(0, 100), error: '' });
    if (rows.some((row) => row.phase === 'complete' && before.get(row.id)?.phase !== 'complete')) refreshSidebarProjections();
  } catch (error) {
    if (revisions.get(backend) === revision && getBackendIdentity(backend).backendId === identity) computers.set(backend, { ...computers.get(backend), error: userFacingError(error) });
  }
}

export function initConversationTransfers(): () => void {
  const stopEvent = wailsEventOn<ConversationTransfer>('thread:transfer', (row, origin) => {
    if (!origin.backendId || !row?.id) return;
    const backend = backendById(origin.backendId);
    if (!backend || getBackendIdentity(backend.id).backendId !== origin.backendId) return;
    receiveTransfer(backend.id, row);
  });
  const stopHello = onBackendHelloChange((key, hello) => {
    if (hello) void refreshComputerTransfers(key);
    else if (!attachedBackends().some((entry) => entry.id === key)) { computers.drop(key); revisions.delete(key); }
  });
  return () => { stopEvent(); stopHello(); };
}

export function resetConversationTransfersForTest(): void {
  computers.reset(); revisions.clear(); pending = null;
}
