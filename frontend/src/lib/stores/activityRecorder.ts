// Tells every attached computer when the person gives this screen input,
// and on which thread, for the time draft (internal/activity; the
// contract is docs/specs/issues.md#activity).
//
// Input is a key, a press, a wheel turn or a touch on the page. Each has
// one passive capture-phase listener whose handler stores a timestamp
// and compares it with the last report's; nothing listens to pointer or
// mouse movement. A report goes out on the first input REPORT_EVERY_MS
// after the last one, and at once when the thread the screen shows
// changes after input (noteActivityFocus), so a click that opens a thread
// is counted on that thread. The computer extends a span while reports on
// one thread keep coming within its window, so a report a minute is the
// whole cost of a working hour: 60 calls, none of them on a turn's path.
//
// `phone` is the compact layout (layoutMode.svelte.ts), the phone shell's
// and a phone browser's. The thread is the focused pane's, or none on the
// compact list. Input the page does not receive (a native browser pane's
// page, the OS's menus) is not seen here; the spec names those limits.

import { RecordActivity } from './bindings';
import { getCompactScreen, isCompactLayout } from './layoutMode.svelte';
import { getFocusedPaneOrNull } from './panes.svelte';
import { isPassiveConnectionFailure } from '../transport/passiveReadFailure';
import { reportFrontendDiagnostic } from '../utils/frontendErrorCapture';
import { userFacingError } from '../utils/userFacingError';

/** The least time between two reports of one thread. Under the
 * computer's window (activity.Window, 3 minutes), so a span never ends
 * between two reports of steady input. */
export const REPORT_EVERY_MS = 60_000;

const INPUTS = ['keydown', 'pointerdown', 'wheel', 'touchstart'] as const;

let lastInput = Number.NEGATIVE_INFINITY;
let lastReport = Number.NEGATIVE_INFINITY;
let reportedThread: string | null = null;
let reportedPhone = false;
let pending = false;

/** The thread this screen shows and whether it is in the compact layout,
 * read reactively, so an effect that calls it follows both. */
function shown(): { thread: string; phone: boolean } {
  const phone = isCompactLayout();
  const thread = phone && getCompactScreen() !== 'thread' ? '' : getFocusedPaneOrNull()?.threadId ?? '';
  return { thread, phone };
}

function onInput(): void {
  const now = Date.now();
  lastInput = now;
  if (!pending && now - lastReport >= REPORT_EVERY_MS) schedule();
}

// The report runs after the input's own handlers, so a press that moves
// focus reports the thread it moved to.
function schedule(): void {
  pending = true;
  setTimeout(send, 0);
}

function send(): void {
  pending = false;
  const { thread, phone } = shown();
  lastReport = Date.now();
  reportedThread = thread;
  reportedPhone = phone;
  RecordActivity(thread, phone).catch((err: unknown) => {
    // A connection that dropped ended its span on the computer, and the
    // next input reports again once it is back. Anything else is a
    // diagnostic, recorded once per distinct failure; the computer's own
    // recording state (GetActivityRecording) is the Time screen's.
    if (isPassiveConnectionFailure(err)) return;
    reportFrontendDiagnostic('An activity report failed', userFacingError(err));
  });
}

/**
 * The thread this screen shows, or its layout, may have changed: report at
 * once when it did and the person gave input within the report interval,
 * which is what moved it. App.svelte runs it from an effect, which follows
 * the pane and layout state it reads.
 */
export function noteActivityFocus(): void {
  const { thread, phone } = shown();
  if (reportedThread === null || (thread === reportedThread && phone === reportedPhone)) return;
  if (!pending && Date.now() - lastInput < REPORT_EVERY_MS) schedule();
}

/** Attach the input listeners; installed once, from App.svelte, which owns
 * the disposer. */
export function installActivityRecorder(): () => void {
  if (typeof window === 'undefined') return () => {};
  const options: AddEventListenerOptions = { capture: true, passive: true };
  for (const name of INPUTS) window.addEventListener(name, onInput, options);
  return () => {
    for (const name of INPUTS) window.removeEventListener(name, onInput, options);
    forgetReports();
  };
}

/** Forget every report, as a page that just loaded has made none. */
function forgetReports(): void {
  lastInput = lastReport = Number.NEGATIVE_INFINITY;
  reportedThread = null;
  reportedPhone = false;
  pending = false;
}

export const resetActivityRecorderForTest = forgetReports;
