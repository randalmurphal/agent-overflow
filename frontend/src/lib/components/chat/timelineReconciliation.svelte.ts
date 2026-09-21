import { tick, untrack } from 'svelte';
import type { UseStickToBottomController } from '../../utils/scroll/types';
import type { TimelineVirtualizerHandle } from '../../utils/virtual/types';
import { reportFrontendDiagnostic } from '../../utils/frontendErrorCapture';

/** History installs retain their correction ownership through row measurement. */
export function installTimelineReconciliation(options: {
  revision(): number;
  getList(): TimelineVirtualizerHandle | undefined;
  stick: UseStickToBottomController;
}): void {
  let observedRevision = untrack(options.revision);
  $effect.pre(() => {
    const revision = options.revision();
    if (revision === observedRevision) return;
    observedRevision = revision;
    const release = options.stick.beginContentReconciliation();
    const abort = new AbortController();
    void tick()
      .then(() => abort.signal.aborted ? undefined : options.getList()?.measureMountedRows(abort.signal))
      .finally(release)
      .catch(error => {
        reportFrontendDiagnostic('timeline: history measurement failed', String(error));
      });
    return () => {
      abort.abort();
      release();
    };
  });
}
