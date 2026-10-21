// What one row of the Pipelines tab shows. The page store projects the
// backend's pipeline summary (and, for rows on screen, its stage
// summaries) into this so the row component renders text and tones only.

export interface PipelineStageView {
  name: string;
  status: string;
}

export interface PipelineRowView {
  /** `pipeline:<repo key>:<id>`, what the page selects by. */
  key: string;
  id: string;
  /** Normalized CI status. */
  status: string;
  /** GitHub's workflow name; '' on GitLab. */
  name: string;
  ref: string;
  refKind: 'branch' | 'tag' | 'merge_request' | string;
  mrNumber: number;
  forge: 'github' | 'gitlab';
  shortSHA: string;
  title: string;
  projectName: string;
  age: string;
  url: string;
  /** Stage names and statuses, read for rows on screen; null until then
   * and while the last read failed. */
  stages: readonly PipelineStageView[] | null;
  /** Why the last stage read failed; null when it answered or none ran. */
  stagesError: string | null;
}

export function shortSHA(sha: string): string {
  return /^[0-9a-f]{12,}$/i.test(sha) ? sha.slice(0, 7) : sha;
}
