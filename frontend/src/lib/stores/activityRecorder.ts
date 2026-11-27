// Tells every attached computer when the person gives this screen input,
// and on which thread, for the time draft (internal/activity; the
// contract is docs/specs/issues.md#activity).
//
// Input is a key, a press, a wheel turn or a touch on the page. Each has
// one passive capture-phase listener whose handler stores a timestamp
// and compares it with the last report's; nothing listens to pointer or
// mouse movement. A report goes out on the first input REPORT_EVERY_MS
// after the last one; input within the interval is reported once it is
// up, with how long ago the last of it was, so a burst's last input
// counts (the computer drops an input older than its window, so a page
// suspended past it adds nothing); and a report goes out at once, aged 0,
// when the thread the screen shows changes after input
// (noteActivityFocus), so a click that opens a thread is counted on that
// thread. Aged 0, it stands for the moment of the change: the time up to
// it stays with the thread shown before, and the time after goes to the
// thread shown now, even when nothing on this screen moved it (another
// screen archived the thread). The computer extends a span while reports on
// one thread keep coming within its window, so a report a minute is the
// whole cost of a working hour: 60 calls, none of them on a turn's path.
//
// `phone` is the compact layout (layoutMode.svelte.ts), the phone shell's
// and a phone browser's. The thread is that of the pane the screen shows
// (paneCommandTarget): on the compact thread screen the pane it shows, a
// companion opened from the Agents rail included, none on the compact
// list, and the focused or promoted pane on the desktop. Input the page
// does not receive (a native browser pane's page, the OS's menus) is not
// seen here; the spec names those limits.

import { RecordActivity } from './bindings';
import { isCompactLayout } from './layoutMode.svelte';
import { getThreadPaneOf, paneCommandTarget } from './panes.svelte';
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
// The report about to go out: at once (after the input's own handlers) or
// when the interval since the last is up.
let pending: ReturnType<typeof setTimeout> | null = null;

/** The thread the screen shows and whether it is in the compact layout,
 * read reactively, so an effect that calls it follows both. */
function shown(): { thread: string; phone: boolean } {
  return { thread: getThreadPaneOf(paneCommandTarget())?.threadId ?? '', phone: isCompactLayout() };
}

function onInput(): void {
  const now = Date.now();
  lastInput = now;
  if (pending === null) schedule(Math.max(0, lastReport + REPORT_EVERY_MS - now), false);
}

// A report at once runs after the input's own handlers, so a press that
// moves focus reports the thread it moved to. A report of a change of
// the shown thread stands for the change (age 0); any other, for the last
// input.
function schedule(delayMs: number, change: boolean): void {
  pending = setTimeout(() => send(change), delayMs);
}

function send(change: boolean): void {
  pending = null;
  const { thread, phone } = shown();
  const now = Date.now();
  lastReport = now;
  reportedThread = thread;
  reportedPhone = phone;
  RecordActivity(thread, phone, change ? 0 : now - lastInput).catch((err: unknown) => {
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
  if (Date.now() - lastInput >= REPORT_EVERY_MS) return;
  if (pending !== null) clearTimeout(pending);
  schedule(0, true);
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

/** Forget every report and drop the one about to go out, as a page that
 * just loaded has made none. */
function forgetReports(): void {
  if (pending !== null) clearTimeout(pending);
  pending = null;
  lastInput = lastReport = Number.NEGATIVE_INFINITY;
  reportedThread = null;
  reportedPhone = false;
}

/** Run after every test of a file that loads this module
 * (src/test/config/testResets.ts), so no report or timer of one test
 * reaches the next. */
export const resetActivityRecorderForTest = forgetReports;
