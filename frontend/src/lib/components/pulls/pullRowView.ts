// What one row of the Pull requests list shows. The page store projects
// the backend's list item (its state, detail and seen record) into this
// so the row component renders text and tones only.

import type { PullSince } from '../../utils/pullsAttention';

export interface PullRowView {
  /** The computer-qualified PR key the page selects by. */
  key: string;
  forge: 'github' | 'gitlab';
  number: number;
  title: string;
  url: string;
  state: string;
  group: string;
  since: PullSince | null;
  /** The app has opened this PR before (a seen record exists). */
  seen: boolean;
  projectName: string;
  /** The author's display name, or "you". */
  author: string;
  /** The row's age ("2h ago"), from the forge's updated time. */
  age: string;
  draft: boolean;
  crossRepository: boolean;
  /** Normalized CI status ('' when the forge has not said yet). */
  ci: string;
  /** Approvals given and required; null until the detail read lands or
   * when the project requires none. */
  approvals: { given: number; required: number } | null;
  /** Unresolved review threads; null until the detail read lands. */
  unresolved: number | null;
}

/** `#163` on GitHub, `!4323` on GitLab. */
export function pullNumberLabel(forge: string, number: number): string {
  return `${forge === 'gitlab' ? '!' : '#'}${number}`;
}
