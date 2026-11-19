import { BrowserCompanionPaneRect, BrowserPaneRect } from './bindings';
import {
  browserPaneMountId,
  noteBrowserActionError,
  observeBrowserCompanion,
  threadForBrowserPaneMount,
} from './browserCompanion.svelte';
import { browserPaneRectKey, createBrowserPaneRectSender, type BrowserPaneRectReport } from './browserPaneRectSender';
import { paneGeometryBridgeAvailable, postPaneGeometry } from '../native/paneGeometryBridge';
import { errString } from '../utils/errors';
import { reportFrontendDiagnostic } from '../utils/frontendErrorCapture';

// Where the thread's mounted browser pane sits, reported by mount: numbered
// per mount, sent to the backend in order (browserPaneRectSender.ts) and
// posted straight to a launcher that takes geometry (paneGeometryBridge.ts).
// A mount's reports end with its attachment.

// The backend's answer when a rect names a mount it no longer has
// (internal/browser SetPaneRect): a teardown race, which the next attach
// re-reports.
const MOUNT_GONE = 'pane mount not found';

const rectSender = createBrowserPaneRectSender({
  send: (mount, rect) => BrowserCompanionPaneRect(mount, new BrowserPaneRect(rect)),
  isMountGone: (error) => errString(error).includes(MOUNT_GONE),
  onFailure: (mount, error) => {
    const threadId = threadForBrowserPaneMount(mount);
    if (threadId) noteBrowserActionError(threadId, `The browser view could not follow the pane: ${errString(error)}`);
  },
});

// Per mount: the newest distinct report, its sequence, and the sequence last
// posted to the launcher (native/paneGeometryBridge.ts).
const paneReports = new Map<string, { key: string; seq: number; postedSeq: number }>();

/**
 * Reports where the mounted pane's host rect sits, already coalesced to one
 * call per frame by the pane. Each distinct report is numbered once per
 * mount. The backend receives it in order through `rectSender`, and its
 * bounds directive carries the number; a launcher that takes geometry
 * directly also receives it at once and moves the view without the backend
 * round trip, placing from whichever of the two is newer.
 */
export function reportBrowserPaneRect(threadId: string, rect: BrowserPaneRectReport): void {
  const mount = browserPaneMountId(threadId);
  if (!mount) return;
  const key = browserPaneRectKey(rect);
  let state = paneReports.get(mount);
  if (!state) {
    state = { key: '', seq: 0, postedSeq: 0 };
    paneReports.set(mount, state);
  }
  if (state.key !== key) {
    state.key = key;
    state.seq += 1;
  }
  const report = { ...rect, seq: state.seq };
  rectSender.report(mount, report);
  if (state.postedSeq === report.seq || !paneGeometryBridgeAvailable()) return;
  try {
    if (postPaneGeometry({ mount, ...report })) state.postedSeq = report.seq;
  } catch (err) {
    // Until a post lands, the launcher places from the backend's directive,
    // which carries this report's sequence; the next report posts again.
    reportFrontendDiagnostic('Browser pane geometry post to the window host failed', errString(err));
  }
}

/** Ends a mount's reports, local and to the backend. */
function forgetPaneMount(mount: string): void {
  rectSender.forget(mount);
  paneReports.delete(mount);
}

observeBrowserCompanion({
  mountEnded: forgetPaneMount,
  resetForTest: () => {
    for (const mount of paneReports.keys()) forgetPaneMount(mount);
  },
});
