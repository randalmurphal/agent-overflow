import type { BrowserCompanionEvent } from './bindings';
import { browserActionsStarted, type BrowserActionFailure } from './browserActions';
import { pageIdOf, sendFromPane, type PageId } from './browserActionWire';
import {
  browserActionError,
  browserPaneAttachment,
  canUseNativeBrowser,
  noteBrowserPaneError,
  observeBrowserCompanion,
  type BrowserPaneAttachment,
} from './browserCompanion.svelte';
import { postPaneFocusIntent } from '../native/paneGeometryBridge';
import { errString } from '../utils/errors';
import { elementFocusScope, intendFocus, type FocusIntent } from '../utils/focusIntent';
import { reportFrontendDiagnostic } from '../utils/frontendErrorCapture';

// Page focus intents (internal/browser/focus_intent.go): the user's request,
// by the address bar's Enter or Escape, that one page take the keyboard. It
// reaches the page a round trip later, and on Windows two, so the next focus
// choice withdraws it first: it is the latest focus intent
// (utils/focusIntent.ts), which a focus anywhere outside the pane's page area
// (the composer, the address bar again), a press outside it or a newer
// deferred focus cancels; and another active tab or the pane unmounting
// withdraws it here. Per thread, the one request in flight;
// numbered per mount, which is how the backend and the launcher tell arms,
// withdrawals and answers apart whatever order they arrive in. A request
// stays here, watched, until one of those choices withdraws it, a newer one
// replaces it, or a Kind "focus" event says how it ended: whatever its arm's
// answer was, and however long it took, since only where focus is applied
// is its age known.
interface PageFocusIntent {
  threadId: string;
  /** The attached pane the request is sent from, whose banner its refusal takes. */
  pane: BrowserPaneAttachment;
  intent: number;
  pageId: PageId;
  /** Its place as the latest focus intent, scoped to the page area. */
  focus: FocusIntent;
  /** browserActionsStarted for the thread when it was armed. */
  actionsAtArm: number;
}

const pageFocusIntents = new Map<string, PageFocusIntent>();
const pageFocusSeqs = new Map<string, number>();

/**
 * Gives keyboard focus to `pageId`, the page the thread's pane shows in
 * `area`, as a browser does after the address bar's Enter or Escape: now if
 * it is presented, or once it is (a restore, a layout), unless the user
 * makes another focus choice first. Only this computer's app window holds
 * the native view (canUseNativeBrowser); anywhere else there is nothing to
 * focus. Unlike a chrome action it leaves a banner already up (the
 * navigation it follows may have failed); a refusal, now or when the page
 * is presented, goes in it unless a newer action's banner is up. Null on
 * success or when there is nothing to focus.
 */
export async function focusBrowserPage(
  threadId: string,
  pageId: string,
  area: HTMLElement,
): Promise<BrowserActionFailure | null> {
  if (!canUseNativeBrowser(threadId)) return null;
  const pane = browserPaneAttachment(threadId);
  const page = pageIdOf(pageId);
  if (!pane || !page) return null;
  const mount = pane.mount;
  // A newer request supersedes the thread's last one wherever it is.
  retirePageFocus(threadId);
  const intent = (pageFocusSeqs.get(mount) ?? 0) + 1;
  pageFocusSeqs.set(mount, intent);
  const record: PageFocusIntent = {
    threadId,
    pane,
    intent,
    pageId: page,
    focus: intendFocus(elementFocusScope(area), () => {
      if (pageFocusIntents.get(threadId) === record) withdrawPageFocus(threadId);
    }),
    actionsAtArm: browserActionsStarted(threadId),
  };
  pageFocusIntents.set(threadId, record);
  try {
    postPaneFocusIntent({ mount, intent, withdrawn: false });
  } catch (err) {
    // The launcher then applies the focus without hearing of this arm; a
    // withdrawal still reaches it, and the backend.
    reportFrontendDiagnostic('Browser page focus post to the window host failed', errString(err));
  }
  const answer = await sendFromPane(pane, { kind: 'focus-page', pageId: page, intent });
  switch (answer.kind) {
    case 'answered':
    case 'pane-gone':
      return null;
    case 'refused':
      // The arm may still have landed: the request stays watched, and the
      // user's next choice withdraws it.
      if (pageFocusIntents.get(threadId) === record && bannerable(record)) noteBrowserPaneError(pane, answer.error);
      return { error: answer.error, cause: answer.cause };
    default:
      return unhandledAnswer(answer);
  }
}

function unhandledAnswer(answer: never): never {
  throw new Error(`Unhandled page focus answer: ${JSON.stringify(answer)}`);
}

/**
 * Whether a refusal of the intent may take the banner: no chrome action
 * started since it was armed, or the one that did left no banner.
 */
function bannerable(record: PageFocusIntent): boolean {
  if (browserActionsStarted(record.threadId) === record.actionsAtArm) return true;
  return !browserActionError(record.threadId);
}

/** Ends the thread's intent here without telling anyone: it landed, failed or was superseded. */
function retirePageFocus(threadId: string): PageFocusIntent | null {
  const record = pageFocusIntents.get(threadId);
  if (!record) return null;
  pageFocusIntents.delete(threadId);
  record.focus.end();
  return record;
}

/**
 * The user made another focus choice: the thread's intent is withdrawn at
 * the launcher (at once, in its own process) and at the backend, so a
 * request still on its way moves nothing, and one that landed meanwhile
 * gives the keyboard back.
 */
function withdrawPageFocus(threadId: string): void {
  const record = retirePageFocus(threadId);
  if (!record) return;
  const { pane, intent } = record;
  try {
    postPaneFocusIntent({ mount: pane.mount, intent, withdrawn: true });
  } catch (err) {
    noteBrowserPaneError(pane, `The browser page may still take the keyboard: ${errString(err)}`);
  }
  // A pane that detached is withdrawn at the backend by its detach; nothing
  // is sent from it.
  void sendFromPane(pane, { kind: 'focus-page-withdraw', intent }).then((answer) => {
    if (answer.kind === 'refused') noteBrowserPaneError(pane, `The browser page may still take the keyboard: ${answer.error}`);
  });
}

/**
 * A Kind "focus" event: how the intent it names ended. Only the thread's
 * current intent settles; a refusal takes the banner unless a newer
 * action's banner is up (bannerable).
 */
function settlePageFocus(event: BrowserCompanionEvent): void {
  const record = pageFocusIntents.get(event.threadId);
  if (!record || record.pane.mount !== event.mount || record.intent !== event.focusIntent) return;
  retirePageFocus(event.threadId);
  if (event.error && bannerable(record)) noteBrowserPaneError(record.pane, event.error);
}

observeBrowserCompanion({
  // Another active tab (a switch by chord, by an agent) is another focus
  // choice than the page asked for.
  stateAdmitted: (event) => {
    const focusIntent = pageFocusIntents.get(event.threadId);
    if (focusIntent && (event.activePageId ?? '') !== focusIntent.pageId) withdrawPageFocus(event.threadId);
  },
  focusSettled: settlePageFocus,
  // Unmounting is a focus choice; the detach withdraws at the backend too.
  mountEnded: (mount) => {
    for (const [threadId, record] of pageFocusIntents) {
      if (record.pane.mount === mount) withdrawPageFocus(threadId);
    }
    pageFocusSeqs.delete(mount);
  },
  resetForTest: () => {
    for (const threadId of pageFocusIntents.keys()) retirePageFocus(threadId);
    pageFocusSeqs.clear();
  },
});
