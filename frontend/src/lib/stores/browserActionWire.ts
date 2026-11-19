import { BrowserCompanionAction, BrowserCompanionDo, type BrowserCompanionEvent } from './bindings';
import {
  admitBrowserState,
  browserStateGeneration,
  isCurrentBrowserPane,
  nativeBrowserRefusal,
  observeBrowserCompanion,
  type BrowserMountId,
  type BrowserPaneAttachment,
} from './browserCompanion.svelte';
import { errString } from '../utils/errors';

// The user's actions on a thread's browser as values, and the one function
// that sends them (sendFromPane, sendForThread). Every BrowserCompanionDo
// call is made here, and each is checked as it is sent, whatever its kind:
// the thread's owner (nativeBrowserRefusal, which the owner moving or
// becoming contested since the gesture turns into a refusal), the pane it
// came from (still the thread's attached mount, else nothing is sent), and
// the connection generation its answer is admitted in. A pane's action is
// numbered in its mount's order here, so no other path can send one
// unnumbered. browserActions.ts says what the user's gestures mean;
// browserPageFocus.ts sends page focus requests.

/** The cookie jars a thread's browser can use (internal/browser/jar.go). */
export type BrowserJar = 'worktree' | 'project' | 'thread' | 'private';

/** A tab's page id: never blank, minted only by pageIdOf. */
export type PageId = string & { readonly __brand: 'PageId' };
/** One of the thread's downloads (ThreadDownload.id): never blank, minted only by downloadIdOf. */
export type DownloadId = string & { readonly __brand: 'DownloadId' };

function isPageId(raw: string): raw is PageId {
  return raw.trim() !== '';
}

function isDownloadId(raw: string): raw is DownloadId {
  return raw.trim() !== '';
}

/** The page id `raw` names, or null for a blank one, which names no page. */
export function pageIdOf(raw: string | null | undefined): PageId | null {
  return raw && isPageId(raw) ? raw : null;
}

/** The download id `raw` names, or null for a blank one. */
export function downloadIdOf(raw: string | null | undefined): DownloadId | null {
  return raw && isDownloadId(raw) ? raw : null;
}

/** What a keyboard command does to the tab active at its turn. */
export type ActiveTabOperation = 'close' | 'reload';

/** The commands that act on the tab they name and select nothing. */
export type NamedTabKind = 'back' | 'forward' | 'reload' | 'stop' | 'restore' | 'close' | 'close-others' | 'close-right' | 'devtools';

/** The download rows' commands. */
export type DownloadKind = 'download-cancel' | 'download-reveal' | 'download-open';

/**
 * One user action on a thread's browser. A command on the active tab names
 * no tab, so the backend resolves the tab in the user's order
 * (internal/browser/active_commands.go); every other command that acts on a
 * tab names it. Built by the constructors in `browserAction`.
 */
export type BrowserAction =
  | { readonly kind: 'active'; readonly operation: ActiveTabOperation }
  | { readonly kind: NamedTabKind; readonly pageId: PageId }
  | { readonly kind: 'navigate'; readonly pageId: PageId; readonly address: string }
  | { readonly kind: 'move'; readonly pageId: PageId; readonly index: number }
  | { readonly kind: 'activate' | 'duplicate'; readonly pageId: PageId }
  | { readonly kind: 'step'; readonly step: 1 | -1 }
  | { readonly kind: 'new'; readonly address: string }
  | { readonly kind: 'reopen' }
  | { readonly kind: 'show-browser' }
  | { readonly kind: 'hide' }
  | { readonly kind: 'jar'; readonly jar: BrowserJar; readonly confirmed: boolean }
  | { readonly kind: DownloadKind; readonly downloadId: DownloadId }
  | { readonly kind: 'downloads-clear' };

/**
 * The actions that make sense with no pane in hand: the chat header's, a
 * mod-clicked link's and a download toast's. Every other kind comes from a
 * pane and is sent only from its attachment (sendFromPane).
 */
export type ThreadAction = Extract<BrowserAction, { readonly kind: 'new' | 'show-browser' | 'hide' | DownloadKind | 'downloads-clear' }>;

/** One constructor per kind of BrowserAction. */
export const browserAction = {
  onActiveTab: (operation: ActiveTabOperation): BrowserAction => ({ kind: 'active', operation }),
  onTab: (kind: NamedTabKind, pageId: PageId): BrowserAction => ({ kind, pageId }),
  navigate: (pageId: PageId, address: string): BrowserAction => ({ kind: 'navigate', pageId, address }),
  move: (pageId: PageId, index: number): BrowserAction => ({ kind: 'move', pageId, index }),
  select: (pageId: PageId): BrowserAction => ({ kind: 'activate', pageId }),
  duplicate: (pageId: PageId): BrowserAction => ({ kind: 'duplicate', pageId }),
  step: (step: 1 | -1): BrowserAction => ({ kind: 'step', step }),
  newTab: (address = ''): ThreadAction => ({ kind: 'new', address }),
  reopen: (): BrowserAction => ({ kind: 'reopen' }),
  showBrowser: (): ThreadAction => ({ kind: 'show-browser' }),
  hide: (): ThreadAction => ({ kind: 'hide' }),
  jar: (jar: BrowserJar, confirmed: boolean): BrowserAction => ({ kind: 'jar', jar, confirmed }),
  download: (kind: DownloadKind, downloadId: DownloadId): ThreadAction => ({ kind, downloadId }),
  clearDownloads: (): ThreadAction => ({ kind: 'downloads-clear' }),
};

/** A page focus intent's arm or withdrawal (browserPageFocus.ts), which only a pane sends. */
export type PageFocusRequest =
  | { readonly kind: 'focus-page'; readonly pageId: PageId; readonly intent: number }
  | { readonly kind: 'focus-page-withdraw'; readonly intent: number };

/** Whether action takes a number in its pane mount's order. */
function ordered(action: BrowserAction | PageFocusRequest): boolean {
  switch (action.kind) {
    case 'active':
    case 'activate':
    case 'step':
    case 'new':
    case 'reopen':
    case 'duplicate':
    case 'download-open':
    case 'show-browser':
      return true;
    case 'download-cancel':
    case 'download-reveal':
    case 'back':
    case 'forward':
    case 'reload':
    case 'stop':
    case 'restore':
    case 'close':
    case 'close-others':
    case 'close-right':
    case 'devtools':
    case 'navigate':
    case 'move':
    case 'hide':
    case 'jar':
    case 'downloads-clear':
    case 'focus-page':
    case 'focus-page-withdraw':
      return false;
    default:
      return unhandled(action);
  }
}

function unhandled(action: never): never {
  throw new Error(`browser action ${JSON.stringify(action)} has no wire form`);
}

/** An ordered action's place in its mount's order. */
interface Place {
  readonly mount: BrowserMountId;
  readonly epoch: number;
  readonly select: number;
}

/** The wire form of action sent from origin, with its place in the mount's order. */
function wireAction(action: BrowserAction | PageFocusRequest, origin: ActionOrigin, place: Place | null): BrowserCompanionAction {
  const base = { kind: '', pageId: '', address: '', index: 0, ...place };
  switch (action.kind) {
    case 'active':
      return new BrowserCompanionAction({ ...base, kind: action.operation === 'close' ? 'close-active' : 'reload-active' });
    case 'back':
    case 'forward':
    case 'reload':
    case 'stop':
    case 'restore':
    case 'close':
    case 'close-others':
    case 'close-right':
    case 'devtools':
    case 'activate':
    case 'duplicate':
      return new BrowserCompanionAction({ ...base, kind: action.kind, pageId: action.pageId });
    case 'navigate':
      return new BrowserCompanionAction({ ...base, kind: 'navigate', pageId: action.pageId, address: action.address });
    case 'move':
      return new BrowserCompanionAction({ ...base, kind: 'move', pageId: action.pageId, index: action.index });
    case 'step':
      return new BrowserCompanionAction({ ...base, kind: 'step', index: action.step });
    case 'new':
      return new BrowserCompanionAction({ ...base, kind: 'new', address: action.address });
    case 'reopen':
    case 'hide':
    case 'downloads-clear':
    case 'show-browser':
      return new BrowserCompanionAction({ ...base, kind: action.kind });
    case 'jar':
      return new BrowserCompanionAction({ ...base, kind: 'jar', jar: action.jar, confirmed: action.confirmed });
    case 'download-cancel':
    case 'download-reveal':
    case 'download-open':
      return new BrowserCompanionAction({ ...base, kind: action.kind, downloadId: action.downloadId });
    case 'focus-page':
    case 'focus-page-withdraw': {
      if (origin.kind !== 'pane') throw new Error('A page focus request is sent from its pane.');
      const pageId = action.kind === 'focus-page' ? action.pageId : '';
      return new BrowserCompanionAction({ ...base, kind: action.kind, pageId, mount: origin.pane.mount, intent: action.intent });
    }
    default:
      return unhandled(action);
  }
}

// Each live mount's numbering (internal/browser/turnorder). The backend
// admits a mount's numbers in order within an epoch and refuses what
// arrives of an older epoch, so a mount starts its next epoch only with
// the first action it sends while none of its own is unanswered: then
// nothing of the older epoch is still meant to act, and a gap the backend
// recorded there (a number lost or overtaken) no longer holds its keyboard
// commands. A new mount (a re-attach) starts again.
interface MountNumbering {
  epoch: number;
  seq: number;
  unanswered: number;
}

const mountNumberings = new Map<BrowserMountId, MountNumbering>();

/** An ordered action's place, and the end of its wait for an answer (safe to call more than once). */
function takePlace(mount: BrowserMountId): { readonly place: Place; readonly answered: () => void } {
  let numbering = mountNumberings.get(mount);
  if (!numbering) {
    numbering = { epoch: 0, seq: 0, unanswered: 0 };
    mountNumberings.set(mount, numbering);
  }
  if (numbering.unanswered === 0) {
    numbering.epoch += 1;
    numbering.seq = 0;
  }
  numbering.seq += 1;
  numbering.unanswered += 1;
  const taken = numbering;
  let waiting = true;
  return {
    place: { mount, epoch: taken.epoch, select: taken.seq },
    answered: () => {
      if (!waiting) return;
      waiting = false;
      taken.unanswered -= 1;
    },
  };
}

/** Where an action is sent from: an attached pane, or its thread with no pane in hand. */
type ActionOrigin =
  | { readonly kind: 'pane'; readonly pane: BrowserPaneAttachment }
  | { readonly kind: 'thread'; readonly threadId: string };

/** What a sent action came to. */
export type BrowserActionAnswer =
  | { readonly kind: 'answered'; readonly state: BrowserCompanionEvent }
  /** Refused here (this page cannot host the thread's browser) or by the backend. */
  | { readonly kind: 'refused'; readonly error: string; readonly cause: unknown }
  /** Its pane detached before it was sent: nothing was sent. */
  | { readonly kind: 'pane-gone' };

/**
 * Sends `action` from `pane`: numbered in the pane mount's order when its
 * kind is ordered, and only while `pane` is still the thread's attached
 * mount.
 */
export function sendFromPane(pane: BrowserPaneAttachment, action: BrowserAction | PageFocusRequest): Promise<BrowserActionAnswer> {
  return send({ kind: 'pane', pane }, action);
}

/** Sends `action` for the thread with no pane in hand: never numbered, it applies in arrival order. */
export function sendForThread(threadId: string, action: ThreadAction): Promise<BrowserActionAnswer> {
  return send({ kind: 'thread', threadId }, action);
}

async function send(origin: ActionOrigin, action: BrowserAction | PageFocusRequest): Promise<BrowserActionAnswer> {
  const threadId = origin.kind === 'pane' ? origin.pane.threadId : origin.threadId;
  const refusal = nativeBrowserRefusal(threadId);
  if (refusal) return { kind: 'refused', error: refusal.message, cause: refusal };
  if (origin.kind === 'pane' && !isCurrentBrowserPane(origin.pane)) return { kind: 'pane-gone' };
  const taken = origin.kind === 'pane' && ordered(action) ? takePlace(origin.pane.mount) : null;
  const generation = browserStateGeneration();
  try {
    const state = await BrowserCompanionDo(threadId, wireAction(action, origin, taken?.place ?? null));
    admitBrowserState(state, generation);
    return { kind: 'answered', state };
  } catch (err) {
    return { kind: 'refused', error: errString(err), cause: err };
  } finally {
    taken?.answered();
  }
}

observeBrowserCompanion({
  // A new mount (a re-attach) numbers its actions afresh.
  mountEnded: (mount) => {
    mountNumberings.delete(mount);
  },
  resetForTest: () => {
    mountNumberings.clear();
  },
});
