// A caught error held for an error surface (toast or pane banner): the report
// plus the backend log lines leading up to it. The log is read when the error
// is captured, not when it is copied, so the lines are taken before they age
// out of the backend's memory and the copy click writes the clipboard without
// awaiting a call.

import { GetErrorLogLines, Version } from './bindings';
import {
  errorReport,
  errorReportText,
  type ErrorReport,
  type ErrorReportContext,
  type ErrorReportEnvironment,
} from '../utils/errorReport';
import { userFacingError } from '../utils/userFacingError';
import { devicePlatform } from '../utils/deviceLabel';
import { hasScope, pageGrantsResolved } from '../transport/scopes';
import { HOME_BACKEND } from '../transport/backendKey';
import { withBackendTarget } from '../transport/backends';
import type { ErrorDetail } from '../transport/errorDetail';

export interface CapturedError {
  readonly report: ErrorReport;
  /** Unset while the log read is in flight, and when there is no log to read. */
  readonly backendLog: ErrorReportEnvironment['backendLog'];
  /** Whether the log read is still in flight. */
  readonly backendLogPending: boolean;
}

type BackendLog = NonNullable<ErrorReportEnvironment['backendLog']>;

let appVersion = '';
let versionRequested = false;

async function requestAppVersion(): Promise<void> {
  if (versionRequested) return;
  versionRequested = true;
  try {
    await pageGrantsResolved();
    if (!hasScope('threads:read', HOME_BACKEND)) throw new Error('Version is not granted to this session');
    appVersion = await Version();
  } catch {
    // A copy without the version is still a useful report, so a failed or
    // ungranted read leaves it out and lets the next capture check again.
    versionRequested = false;
  }
}

// The log of the backend that answered the failed call, read from that
// backend. Its lines cover whatever the backend did, so a session reads
// them only with the grant that already lets it run an agent there.
async function readBackendLog(detail: ErrorDetail): Promise<BackendLog | undefined> {
  try {
    if (detail.backend === HOME_BACKEND) await pageGrantsResolved();
    if (!hasScope('threads:operate', detail.backend)) return undefined;
    const result = await withBackendTarget(detail.backend, () => GetErrorLogLines(detail.ref));
    return { lines: result.lines ?? [], found: result.found };
  } catch (err) {
    return { unavailable: `could not be read (${userFacingError(err)})` };
  }
}

export function captureError(err: unknown, ctx: ErrorReportContext = {}): CapturedError {
  const report = errorReport(err, ctx);
  const captured = $state<{ report: ErrorReport; backendLog: ErrorReportEnvironment['backendLog']; backendLogPending: boolean }>({
    report,
    backendLog: undefined,
    backendLogPending: report.detail !== undefined,
  });
  void requestAppVersion();
  if (report.detail) {
    void readBackendLog(report.detail).then((log) => {
      captured.backendLog = log;
      captured.backendLogPending = false;
    });
  }
  return captured;
}

/** The captured error as text to paste into an agent. */
export function capturedErrorText(captured: CapturedError): string {
  const backendLog = captured.backendLog
    ?? (captured.backendLogPending ? { unavailable: 'still being read when this was copied' } : undefined);
  return errorReportText(captured.report, {
    appVersion: appVersion || undefined,
    platform: devicePlatform() || undefined,
    backendLog,
  });
}

/** Test-only: forget the cached app version. */
export function resetErrorReportsForTest(): void {
  appVersion = '';
  versionRequested = false;
}
