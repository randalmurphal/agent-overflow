// The chip on a user row an agent in another thread wrote.
//
// `meta.origin = "agent-thread"` with `meta.originThread` is what the thread
// tools stamp on a spawn's first message, on every thread_send, and on every
// wake carrying an answer back (docs/specs/agent-thread-tools.md,
// Attribution). The chip says "from <title>", adds "on <computer>" when the
// source is a different computer than the one showing the row, and opens
// that thread when this client is attached to its computer and that socket
// is up. Offline, unpaired or unknown, it reads the same and does nothing:
// a dead link is worse than a label.

import { getAttachedBackends, attachedBackendEntry, backendDisplayName, backendReachable } from '../../stores/attachedBackends.svelte';
import { getThreadById } from '../../stores/threads.svelte';
import { openThreadInPane, findPaneShowingThread, focusPane, revealPane } from '../../stores/panes.svelte';
import { GetThread } from '../../stores/bindings';
import { withBackendTarget } from '../../transport/backends';
import type { BackendKey } from '../../transport/backendKey';
import { noteThread } from '../../transport/entityIndex';
import { addErrorToast } from '../../stores/toast.svelte';
import { errString } from '../../utils/errors';
import type { Thread } from '../../types/models';
import type { UserMessageOriginThread } from '../../utils/userMessageMeta';

export interface AgentThreadOriginChip {
  label: string;
  /** Null for the inert chip: same label, no affordance. */
  open: (() => void) | null;
}

/**
 * The registry entry for the computer an origin names, by registry id first
 * and by the stable computer UUID second. Both spellings reach this side:
 * the id is what a pairing profile carries, the UUID is what identifies the
 * same computer across reconnects.
 */
function originBackend(computerId: string) {
  if (computerId === '') return undefined;
  const byKey = attachedBackendEntry(computerId);
  if (byKey) return byKey;
  for (const entry of getAttachedBackends()) {
    if (entry.backendId === computerId) return entry;
  }
  return undefined;
}

/**
 * The chip for one origin, as rendered on a row shown by `viewer`.
 *
 * An origin with no computer, or one that resolves to the computer showing
 * the row, is local: no "on" suffix. Anything else names its computer, by
 * this client's display name for it when it knows the computer and by the
 * name the wire carried when it does not.
 */
export function agentThreadOriginChip(
  origin: UserMessageOriginThread,
  viewer: BackendKey,
  place?: (thread: Thread) => Promise<unknown>,
): AgentThreadOriginChip {
  const title = origin.title || 'another thread';
  const { computer, open } = agentThreadOpener(origin, viewer, place);
  return { label: computer ? `from ${title} on ${computer}` : `from ${title}`, open };
}

export interface AgentThreadOpener {
  /** The thread's computer by name when it is not `viewer`, else ''. */
  computer: string;
  /** Null when this client cannot reach that computer. */
  open: (() => void) | null;
}

/**
 * How a row shown by `viewer` reaches a thread another agent named: the
 * computer to name beside it, and the open action when that computer is
 * attached and up. Shared by the origin chip, the AO tool rows' links and
 * the Agents tray, which passes `place` to open it in its companion pane.
 */
export function agentThreadOpener(
  origin: UserMessageOriginThread,
  viewer: BackendKey,
  place?: (thread: Thread) => Promise<unknown>,
): AgentThreadOpener {
  const entry = originBackend(origin.computerId);
  const elsewhere = origin.computerId !== '' && entry?.id !== viewer;
  const computer = elsewhere ? (entry ? backendDisplayName(entry) : origin.computerName) : '';
  if (!entry && origin.computerId !== '') return { computer, open: null };
  const backend = entry?.id ?? viewer;
  if (!backendReachable(backend)) return { computer, open: null };
  return { computer, open: () => { void openAgentThreadOrigin(origin, backend, place); } };
}

/**
 * Open the source thread in a pane, by default the focused one, otherwise
 * where `place` puts it. The row is read from its own computer
 * when this client does not already hold it, and the read teaches the RPC
 * router which computer owns the id, so the pane's own calls route there.
 */
export async function openAgentThreadOrigin(
  origin: UserMessageOriginThread,
  backend: BackendKey,
  place: (thread: Thread) => Promise<unknown> = openThreadInPane,
): Promise<void> {
  try {
    const open = findPaneShowingThread(origin.threadId);
    if (open) {
      focusPane(open.paneId);
      revealPane(open.paneId);
      return;
    }
    let thread = getThreadById(origin.threadId);
    if (!thread) {
      const row = (await withBackendTarget(backend, () => GetThread(origin.threadId))) as Thread | null;
      if (!row?.id) throw new Error('That thread is no longer on that computer.');
      noteThread(row.id, backend, row.ownershipEpoch ?? 0);
      thread = row;
    }
    await place(thread);
  } catch (err) {
    addErrorToast(`Could not open ${origin.title || 'that thread'}: ${errString(err)}`, err);
  }
}
