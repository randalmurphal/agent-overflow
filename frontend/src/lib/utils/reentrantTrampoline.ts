import { reportFrontendDiagnostic } from './frontendErrorCapture';

/**
 * Laps one entry into a trampoline may run before it is abandoned.
 *
 * Same reasoning as `timelineQuietWork`'s cap and svelte's flush caps: the
 * argued legitimate depth is 1-2 (a pass re-entered once by work it itself
 * scheduled), so this leaves well over an order of magnitude on top. Tripping
 * it is a bug report, not a tuning parameter.
 */
export const MAX_TRAMPOLINE_LAPS = 64;

/**
 * A re-entrancy trampoline with a lap cap.
 *
 * The shape it exists for: a function that can be called again from inside its
 * own body, where running a NESTED pass would be wrong — the outer pass would
 * finish afterwards and overwrite state the nested one computed from fresher
 * input. The nested call therefore just raises a flag and returns, and the
 * outer call re-runs the pass until the flag stays clear.
 *
 * "Until the flag stays clear" is an unbounded synchronous loop, and a pass
 * that re-enters on every lap is the renderer-freeze signature this codebase
 * has been bitten by: one core pegged, no paint, no error, nothing in any log.
 * The cap abandons the loop and reports instead. Abandoning is the right
 * answer, not a compromise — a pass that has re-entered 64 times is not
 * converging, and the state it would have settled on is worth less than a
 * responsive main thread and a record of why.
 *
 * The budget is per ENTRY, deliberately: the trampoline's driver is external
 * (wire events), so a sticky stand-down would disable the pass for the rest of
 * the session on the strength of one bad input. Contrast `timelineQuietWork`,
 * whose driver is its own timer and whose stand-down therefore must be sticky.
 */
export function createReentrantTrampoline(
  /** Names the loop in the diagnostic detail. Constant per call site. */
  name: string,
  pass: () => void,
  maxLaps: number = MAX_TRAMPOLINE_LAPS,
): () => void {
  let running = false;
  let again = false;

  return function enter(): void {
    if (running) {
      again = true;
      return;
    }
    running = true;
    try {
      let laps = 0;
      do {
        again = false;
        pass();
        laps += 1;
        if (again && laps >= maxLaps) {
          // Constant message, variables in `detail`: a loop name and a lap
          // count in the message would mint a signature per call site and per
          // count, which is unbounded map growth in the capture pipeline and a
          // walk around its per-signature cap. Console too — a remote session
          // cannot persist (`ReportFrontendErrorBatch` is host-scoped), and the
          // console line is then the only surviving evidence.
          const detail = `${name}, ${laps} laps`;
          console.warn(
            `[reentrantTrampoline] pass re-entered on every lap; loop abandoned (${detail})`,
          );
          reportFrontendDiagnostic(
            'reentrantTrampoline: a re-entrant pass re-entered on every lap without converging; ' +
              'the loop was abandoned to release the main thread',
            detail,
          );
          return;
        }
      } while (again);
    } finally {
      // Both flags are per-entry state; the next external call starts clean
      // whether this one converged or was abandoned.
      running = false;
      again = false;
    }
  };
}
