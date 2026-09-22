import { BeginDraftProjectTransfer, BindThreadTransferDestination, CreateThreadTransferOffer, GetThreadTransfers, GetThread, GetDraft } from './bindings';
import { withBackendTarget, requireEntityBackend } from '../transport/backends';
import { projectBackend, resolveThreadBackend, noteThread } from '../transport/entityIndex';
import { getBackendIdentity } from '../transport/backendIdentity';
import type { Project, Thread } from '../types/models';
import type { Draft, DraftSnapshot } from '../types/draft';
import { draftTextAndContextMatch } from './composerDraftSnapshots';
import { randomId } from '../utils/randomId';
import { getTransportHelloFor } from './transportStatus.svelte';

/** Reuses the durable host-to-host copy protocol, including attachment files. */
export async function moveDraftToComputer(source: Thread, project: Project, expected: DraftSnapshot): Promise<Thread> {
  const from = requireEntityBackend(resolveThreadBackend(source.id));
  const to = requireEntityBackend(projectBackend(project.id));
  const destinationID = getBackendIdentity(to).backendId;
  if (![from, to].every((backend) => getTransportHelloFor(backend)?.capabilities.includes('draft.project-transfer.v1'))) {
    throw new Error('Update both computers before moving a draft between projects.');
  }
  if (!destinationID) throw new Error('Waiting for the destination computer to identify itself.');
  const operationID = randomId();
  const intent = await withBackendTarget(from, () => BeginDraftProjectTransfer(source.id, operationID, destinationID, expected));
  const offer = await withBackendTarget(to, () => CreateThreadTransferOffer(intent, project.id, '', ''));
  await withBackendTarget(from, () => BindThreadTransferDestination(source.id, offer));
  const deadline = Date.now() + 300_000;
  while (Date.now() < deadline) {
    const rows = await withBackendTarget(from, () => GetThreadTransfers());
    const row = rows.find((entry) => entry.id === operationID);
    if (!row) throw new Error('The draft transfer could not be found. The source draft is retained.');
    if (row.error || row.phase === 'canceled') throw new Error(row.error || 'The draft transfer was canceled.');
    if (row.phase === 'complete') {
      noteThread(row.targetThreadId, to);
      const [thread, draft] = await withBackendTarget(to, () => Promise.all([GetThread(row.targetThreadId), GetDraft(row.targetThreadId)])) as [Thread, Draft];
      if (!draftTextAndContextMatch(draft, { ...expected, sourceProposedPlan: null }) || draft.attachmentIds.length !== expected.attachmentIds.length) {
        throw new Error('The draft moved, but its destination changed before it could be opened. Open it from the destination project.');
      }
      return thread;
    }
    await new Promise((resolve) => setTimeout(resolve, 250));
  }
  throw new Error('The draft is still moving. Check its progress under conversation transfers.');
}
