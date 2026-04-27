// Forge-aware label resolution. The frontend keeps internal stage
// names (`pr.review`, `pr.create`, etc.) stable across forges — only
// user-visible strings switch between PR / MR conventions. A single
// helper avoids scattering ternaries through templates and makes the
// per-forge mapping testable in isolation.

export interface ForgeLabels {
  /** Short noun: "PR" / "MR". */
  noun: 'PR' | 'MR';
  /** Action verb phrase: "Create PR" / "Create MR". */
  createAction: string;
  /** Long form for sentences: "Pull request" / "Merge request". */
  longSingular: string;
  /** Step / button label: "Open PR" / "Open MR". */
  openAction: string;
  /** Sentence-case noun pair for headers: "Pull Request" / "Merge Request". */
  longSingularTitleCase: string;
  /** Sigil used in URL/short-form refs: "#" (github) / "!" (gitlab). */
  numberSigil: '#' | '!';
}

const githubLabels: ForgeLabels = {
  noun: 'PR',
  createAction: 'Create PR',
  longSingular: 'Pull request',
  openAction: 'Open PR',
  longSingularTitleCase: 'Pull Request',
  numberSigil: '#',
};

const gitlabLabels: ForgeLabels = {
  noun: 'MR',
  createAction: 'Create MR',
  longSingular: 'Merge request',
  openAction: 'Open MR',
  longSingularTitleCase: 'Merge Request',
  numberSigil: '!',
};

/**
 * Resolves user-visible labels for a forge id. Falls back to GitHub
 * strings for unknown / empty forge values so the UI never renders a
 * blank label — visibility / disabled state is controlled separately
 * (callers gate on `status.forge !== ''`).
 */
export function forgeLabels(forge: string | null | undefined): ForgeLabels {
  if (forge === 'gitlab') {
    return gitlabLabels;
  }
  return githubLabels;
}
