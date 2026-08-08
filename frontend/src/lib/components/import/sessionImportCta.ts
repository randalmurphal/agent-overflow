// The import modal's one primary button, resolved.
//
// Plain TS in the sessionImportKeyboard.ts style: the button morphs through
// four meanings (progress readout, retry-the-failures, import-the-selection,
// import-everything-shown) and each one changes BOTH the label and the ids
// the click acts on. Keeping the pair in one pure function is what stops a
// label from ever describing a different set than the one that gets
// imported.

import type { SessionImportStatus } from '../../stores/sessionImport.svelte';

/** Just enough of the run for the CTA; the modal passes the store's. */
export interface ImportCtaRun {
  active: boolean;
  completed: number;
  total: number;
}

export interface ImportCtaInput {
  status: SessionImportStatus;
  run: ImportCtaRun | null;
  /** A view-only session cannot import at all. */
  viewOnly: boolean;
  /**
   * Failed rows of a SETTLED run — the store returns none while a run is in
   * flight, so a non-empty list is itself the "offer a retry" condition.
   */
  failedIds: readonly string[];
  selection: ReadonlySet<string>;
  /** Ids currently surviving the filters, in catalog order. */
  filteredIds: ReadonlySet<string>;
}

export interface ImportCta {
  /** Rows the click imports. */
  targetIds: string[];
  label: string;
  enabled: boolean;
}

export function resolveImportCta(input: ImportCtaInput): ImportCta {
  const { status, run, viewOnly, failedIds, selection, filteredIds } = input;
  const runActive = run?.active === true;

  // A settled run that left failures behind owns the button: the rows are
  // stamped, and the only thing to do with them is retry those and nothing
  // else. Closing (Cancel) drops the stamps and returns the button to its
  // normal meaning.
  const retrying = failedIds.length > 0;
  // "Import all" acts on what the filters currently show; an explicit
  // selection wins and survives filter changes.
  const targetIds = retrying
    ? [...failedIds]
    : selection.size > 0
      ? [...selection]
      : [...filteredIds];

  let label: string;
  if (runActive && run) label = `Importing ${run.completed} of ${run.total}…`;
  else if (retrying) label = `Retry failed (${failedIds.length})`;
  else if (selection.size > 0) label = `Import (${targetIds.length})`;
  else label = `Import all (${targetIds.length})`;

  return {
    targetIds,
    label,
    enabled: !viewOnly && !runActive && status === 'ready' && targetIds.length > 0,
  };
}
