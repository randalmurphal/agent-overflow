// A surface hosted in a layout pane that has views of its own to step out
// of before the pane goes: the review's CI log, conflict view and, on
// compact, the open file-tree rail. The hosted surface registers its step
// under the layout pane hosting it; the pane's Back (the companion rung of
// `utils/stepBack.ts`, the Pull requests page's `stepBack`) runs it first.

type BackStep = () => boolean;

const steps = new Map<string, BackStep>();

/** Registers `step` for `paneId`; the disposer removes it only while it
 * is still the registered step, so a remount's successor survives. */
export function registerPaneBackStep(paneId: string, step: BackStep): () => void {
  steps.set(paneId, step);
  return () => {
    if (steps.get(paneId) === step) steps.delete(paneId);
  };
}

/** Runs the step hosted in `paneId`; false when none is registered or it
 * had nothing open to close. */
export function stepPaneBack(paneId: string): boolean {
  return steps.get(paneId)?.() ?? false;
}
