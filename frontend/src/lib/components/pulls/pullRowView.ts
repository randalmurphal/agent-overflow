// What one row of the Pull requests list shows. The page store projects
// the backend's list item (its state, reason and facts) into this so the
// row component renders text and tones only.

import type { ReadinessTone } from '../../utils/reviewReadiness';

export interface PullRowView {
  /** The computer-qualified PR key the page selects by. */
  key: string;
  forge: 'github' | 'gitlab';
  number: number;
  title: string;
  url: string;
  state: string;
  group: string;
  /** The state label, worded with its reason (`pullStateText`). */
  label: string;
  tone: ReadinessTone;
  projectName: string;
  /** The author's display name, or "you". */
  author: string;
  viewerIsAuthor: boolean;
  /** The row's age ("2h"), from the forge's updated time. */
  age: string;
  draft: boolean;
  crossRepository: boolean;
  /** Normalized CI status of the head ('' when the forge has not said). */
  ci: string;
  /** Approvals given and required; null when unknown or none required. */
  approvals: { given: number; required: number } | null;
  /** Unresolved review threads; null when unknown. */
  unresolved: number | null;
}

/** `#163` on GitHub, `!4323` on GitLab. */
export function pullNumberLabel(forge: string, number: number): string {
  return `${forge === 'gitlab' ? '!' : '#'}${number}`;
}
