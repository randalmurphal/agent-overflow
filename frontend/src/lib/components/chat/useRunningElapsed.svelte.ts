import { formatElapsedSeconds } from '../../utils/format';

/**
 * Shared running-elapsed ticker for tool-row headers (AdvisorRow,
 * AgentRow, GenericToolCallRow). Three rows previously held
 * structurally-identical `now = $state`, `setInterval` $effect, and
 * threshold-gated label derivations. Centralising the contract here
 * means a future change to the 2s warm-up threshold, the 1s tick
 * interval, or the SSR/test exception lands in one place.
 *
 * Usage (must be called from a `.svelte` component, since it uses
 * runes):
 *
 *   const ticker = createRunningElapsed(
 *     () => item.status === 'running' || item.status === 'streaming',
 *     () => item.createdAt,
 *   );
 *   const label = $derived(ticker.label);
 *
 * The threshold guard hides the label entirely during the first 2s
 * so a fast tool call doesn't show "0s" → "1s" jitter before
 * resolving. Callers can still gate downstream logic on
 * `ticker.label === ''` if they want to hide the duration cell.
 */
export const RUNNING_ELAPSED_THRESHOLD_MS = 2_000;

interface RunningElapsed {
  readonly label: string;
}

export function createRunningElapsed(
  isTicking: () => boolean,
  createdAt: () => number,
  thresholdMs: number = RUNNING_ELAPSED_THRESHOLD_MS,
): RunningElapsed {
  let now = $state(Date.now());
  $effect(() => {
    if (!isTicking()) return;
    now = Date.now();
    const id = setInterval(() => {
      now = Date.now();
    }, 1_000);
    return () => clearInterval(id);
  });

  const label = $derived.by<string>(() => {
    if (!isTicking()) return '';
    const created = createdAt();
    if (!Number.isFinite(created) || created <= 0) return '';
    const elapsedMs = now - created;
    if (elapsedMs < thresholdMs) return '';
    return formatElapsedSeconds(Math.floor(elapsedMs / 1_000));
  });

  return {
    get label() {
      return label;
    },
  };
}
