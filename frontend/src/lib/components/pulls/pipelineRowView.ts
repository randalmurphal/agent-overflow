// What one row of the Pipelines tab shows. The page store projects the
// backend's pipeline summary (and, for rows on screen, its stage
// summaries) into this so the row component renders text and tones only.

import { absoluteDateTime } from './pullRowView';

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
  /** The head commit's sha as the forge sent it. */
  sha: string;
  shortSHA: string;
  title: string;
  projectName: string;
  /** The age from the pipeline's created time ("5m ago"). */
  age: string;
  /** Both times, absolute: the age's hover text and accessible label. */
  times: string;
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

/** "Created <date and time> · Updated <date and time>". */
export function pipelineTimesText(createdAt: string, updatedAt: string): string {
  const created = absoluteDateTime(Date.parse(createdAt));
  const updated = absoluteDateTime(Date.parse(updatedAt));
  return [created ? `Created ${created}` : '', updated ? `Updated ${updated}` : ''].filter((part) => part !== '').join(' · ');
}
