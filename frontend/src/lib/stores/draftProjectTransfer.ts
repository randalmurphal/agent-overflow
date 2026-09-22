import { BeginDraftProjectTransfer, BindThreadTransferDestination, CreateThreadTransferOffer, GetThreadTransferStatus, GetThread } from './bindings';
import { withBackendTarget, requireEntityBackend } from '../transport/backends';
import { projectBackend, resolveThreadBackend, noteThread } from '../transport/entityIndex';
import { getBackendIdentity } from '../transport/backendIdentity';
import type { Project, Thread } from '../types/models';
import type { DraftSnapshot } from '../types/draft';
import { addToast } from './toast.svelte';
import { userFacingError } from '../utils/userFacingError';
import { randomId } from '../utils/randomId';
import { getTransportHelloFor } from './transportStatus.svelte';

/** Reuses the durable host-to-host copy protocol, including attachment files. */
export async function moveDraftToComputer(source: Thread, project: Project, expected: DraftSnapshot): Promise<Thread> {
  const from = requireEntityBackend(resolveThreadBackend(source.id));
  const to = requireEntityBackend(projectBackend(project.id));
  const destinationID = getBackendIdentity(to).backendId;
  if (![from, to].every((backend) => getTransportHelloFor(backend)?.capabilities.includes('draft.project-transfer.v2'))) {
    throw new Error('Update both computers before moving a draft between projects.');
  }
  if (!destinationID) throw new Error('Waiting for the destination computer to identify itself.');
  const operationID = randomId();
  const intent = await withBackendTarget(from, () => BeginDraftProjectTransfer(source.id, operationID, destinationID, expected));
  const offer = await withBackendTarget(to, () => CreateThreadTransferOffer(intent, project.id, '', ''));
  await withBackendTarget(from, () => BindThreadTransferDestination(source.id, offer));
  const deadline = Date.now() + 300_000;
  while (Date.now() < deadline) {
    const row = await withBackendTarget(from, () => GetThreadTransferStatus(source.id, operationID));
    if (row.phase === 'complete') {
      if (row.error) addToast('warning', `Draft moved; cleanup will retry: ${userFacingError(row.error)}`);
      noteThread(row.targetThreadId, to);
      return await withBackendTarget(to, () => GetThread(row.targetThreadId)) as Thread;
    }
    if (row.error || row.phase === 'canceled') throw new Error(row.error || 'The draft transfer was canceled.');
    await new Promise((resolve) => setTimeout(resolve, 250));
  }
  throw new Error('The draft is still moving. Check its progress under conversation transfers.');
}
