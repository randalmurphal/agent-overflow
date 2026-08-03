// Display copy for the provider's live fast-mode report.
//
// The wire values are undocumented, version-dependent Claude CLI enums,
// so this module is the single place they turn into human text — one
// mapping, one passthrough default, no inline string tables at call
// sites. A value we have never seen renders as itself (dashes → spaces)
// rather than disappearing: the user seeing an unfamiliar reason is
// strictly better than the UI silently claiming fast mode is fine.

/** `fast_mode_state` values observed on Claude 2.1.219. */
export type FastModeWireState = 'on' | 'off' | 'cooldown' | (string & {});

export interface FastModeReport {
  /** Empty when the provider reported a reason but no state. */
  state: FastModeWireState;
  /** Empty on older CLIs — the field did not exist before ~2.1.219. */
  disabledReason: string;
}

// Keep in sync with the enum documented in
// docs/references/claude-wire.md §fast_mode_state. Phrased as the answer
// to "why isn't fast mode running?" so each line reads as a sentence
// after "Fast mode is off: ".
const DISABLED_REASON_COPY: Record<string, string> = {
  not_first_party: 'this account is not on a first-party Anthropic backend',
  disabled_by_env: 'disabled by an environment variable',
  model_not_allowed: 'this model does not support it',
  sdk_opt_in_required: 'the session did not opt in',
  extra_usage_disabled: 'extra usage is turned off for this account',
  preference: 'turned off in your Claude settings',
  free: 'not available on this plan',
  pending: 'still being enabled',
  network_error: 'the provider could not reach the service',
  unknown: 'the provider did not say why',
};

/** Human copy for a `fast_mode_disabled_reason`, or '' when absent. */
export function fastModeReasonText(reason: string): string {
  const key = reason.trim();
  if (key === '') return '';
  return DISABLED_REASON_COPY[key] ?? key.replace(/_/g, ' ');
}

/**
 * True when the thread asks for fast mode but the provider says it is not
 * actually running.
 *
 * Deliberately requires a KNOWN non-'on' state. A thread with no report
 * yet (older CLI, no turn finished, non-Claude provider) is unknown, not
 * disabled — claiming otherwise would be the same lie in the opposite
 * direction.
 */
export function isFastModeContradicted(
  requested: boolean,
  report: FastModeReport | undefined,
): boolean {
  if (!requested || !report) return false;
  return report.state === 'off' || report.state === 'cooldown';
}

/**
 * One-line explanation of a contradicted fast-mode toggle, for hover
 * text. Returns '' when there is nothing honest to say.
 */
export function fastModeContradictionText(report: FastModeReport | undefined): string {
  if (!report) return '';
  const reason = fastModeReasonText(report.disabledReason);
  if (report.state === 'cooldown') {
    return reason
      ? `Provider paused fast mode after a rate limit: ${reason}`
      : 'Provider paused fast mode after a rate limit';
  }
  if (report.state === 'off') {
    return reason ? `Provider reports fast mode off: ${reason}` : 'Provider reports fast mode off';
  }
  return '';
}
