// A caught error held for an error surface (toast or pane banner): the report
// plus the backend log lines leading up to it. The log is read when the error
// is captured, not when it is copied, so the lines are taken before they age
// out of the backend's memory and the copy click writes the clipboard without
// awaiting a call.

import { GetErrorLogLines, Version } from './bindings';
import {
  errorHasBackendLog,
  errorReport,
  errorReportText,
  type ErrorReport,
  type ErrorReportContext,
  type ErrorReportEnvironment,
} from '../utils/errorReport';
import { userFacingError } from '../utils/userFacingError';
import { devicePlatform } from '../utils/deviceLabel';

export interface CapturedError {
  readonly report: ErrorReport;
  /** Unset while the log read is in flight, and when there is no log to read. */
  readonly backendLog: ErrorReportEnvironment['backendLog'];
}

let appVersion = '';
let versionRequested = false;

async function requestAppVersion(): Promise<void> {
  if (versionRequested) return;
  versionRequested = true;
  try {
    appVersion = await Version();
  } catch {
    // A copy without the version is still a useful report, so a failed
    // read leaves it out and lets the next capture try again.
    versionRequested = false;
  }
}

async function readBackendLog(ref: string): Promise<NonNullable<ErrorReportEnvironment['backendLog']>> {
  try {
    const result = await GetErrorLogLines(ref);
    return { lines: result.lines ?? [], found: result.found };
  } catch (err) {
    return { unavailable: `could not be read (${userFacingError(err)})` };
  }
}

export function captureError(err: unknown, ctx: ErrorReportContext = {}): CapturedError {
  const report = errorReport(err, ctx);
  const captured = $state<{ report: ErrorReport; backendLog: ErrorReportEnvironment['backendLog'] }>({
    report,
    backendLog: undefined,
  });
  void requestAppVersion();
  if (report.detail && errorHasBackendLog(report)) {
    void readBackendLog(report.detail.ref).then((log) => { captured.backendLog = log; });
  }
  return captured;
}

/** The captured error as text to paste into an agent. */
export function capturedErrorText(captured: CapturedError): string {
  const backendLog = captured.backendLog
    ?? (captured.report.detail && errorHasBackendLog(captured.report)
      ? { unavailable: 'still being read when this was copied' }
      : undefined);
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
