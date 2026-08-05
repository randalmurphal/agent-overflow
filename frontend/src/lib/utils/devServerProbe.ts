// Liveness gate for the dev-server chip. Triage's textual detection
// (internal/triage/dev_server_url.go) proves only that command output
// mentioned a loopback URL; whether a server is actually listening is
// answered by the backend (ProbeDevServerURL → internal/devserverprobe,
// which owns the verdict TTLs — this module deliberately keeps no
// verdict cache of its own, only in-flight dedup, so two layers can
// never disagree about staleness).
//
// A view-only session short-circuits before the wire: the RPC is
// loopback-only, and a remote viewer's localhost is not the backend's,
// so the chip has nothing truthful to offer there. Past that guard a
// rejection is a real transport fault — logged, then degraded to "not
// live", because the chip is an affordance, not an error surface.
import { ProbeDevServerURL } from '../stores/bindings';
import { isViewOnlySession } from '../transport/runMode';

/** Retry cadence while a command still runs and its candidate is unconfirmed. */
export const DEV_SERVER_PROBE_RETRY_MS = 1_500;
/** Re-verify cadence for a confirmed URL while its command still runs. */
export const DEV_SERVER_PROBE_VERIFY_MS = 5_000;
/** Consecutive dead verdicts after which a running row stops retrying (~30s). */
export const DEV_SERVER_PROBE_MAX_DEAD_PROBES = 20;
// Both cadences sit strictly above the backend's verdict TTLs (dead 1s,
// live 3s — internal/devserverprobe/probe.go), so every scheduled probe
// reaches the dialer instead of a memoized verdict.

const inFlight = new Map<string, Promise<boolean>>();

/** Whether a server is currently listening on this loopback URL. */
export function probeDevServerURL(url: string): Promise<boolean> {
  if (isViewOnlySession()) return Promise.resolve(false);
  const pending = inFlight.get(url);
  if (pending) return pending;

  const probe = ProbeDevServerURL(url)
    .then(
      (live) => live,
      (err) => {
        console.warn('dev-server probe failed:', url, err);
        return false;
      },
    )
    .finally(() => inFlight.delete(url));
  inFlight.set(url, probe);
  return probe;
}

/** Test-only: drop in-flight dedup state so cases don't bleed into each other. */
export function resetDevServerProbeForTest(): void {
  inFlight.clear();
}
