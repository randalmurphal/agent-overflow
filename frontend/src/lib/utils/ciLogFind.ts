// The review pane's find over an open CI job log: the backend counts the
// matches per line (SearchPRCILog); this maps them onto the log view's
// rows. A row of lines is at most one block of one section
// (ReviewCILogView), so a match names the section to open, the row that
// shows it (`data-find-key`) and its index among that row's matches.
// Lines no section lists are not on the view and are not matches. An
// answer applies only to the job and log version it was read from.

import { CI_LOG_BLOCK_LINES } from '../stores/ciLogBlocks.svelte';
import type { CILogSearch, CILogSection } from '../types/models';

/** `data-find-key` of a row of log lines, by its first line. */
export function findCILogKey(from: number): string {
  return `cl:${from}`;
}

export interface CILogFindHit {
  /** The line and the match's index on it. */
  readonly identity: string;
  readonly rowKey: string;
  readonly occurrence: number;
  readonly line: number;
  readonly sectionKey: string;
  /** The row's first line. */
  readonly rowFrom: number;
}

/** The view rows' matches for a search answer, in line order; at most `limit`. */
export function ciLogFindHits(search: CILogSearch, sections: readonly CILogSection[], limit: number): CILogFindHit[] {
  const out: CILogFindHit[] = [];
  const shown = sections.filter((section) => section.end > section.start).sort((a, b) => a.start - b.start);
  let at = 0;
  let rowFrom = -1;
  let inRow = 0;
  for (const hit of search.hits) {
    while (at < shown.length && shown[at].end <= hit.line) at += 1;
    const section = shown[at];
    if (!section || section.start > hit.line) continue;
    const from = Math.max(section.start, Math.floor(hit.line / CI_LOG_BLOCK_LINES) * CI_LOG_BLOCK_LINES);
    if (from !== rowFrom) {
      rowFrom = from;
      inRow = 0;
    }
    for (let occurrence = 0; occurrence < hit.count; occurrence += 1) {
      if (out.length >= limit) return out;
      out.push({
        identity: `${hit.line}:${occurrence}`,
        rowKey: findCILogKey(from),
        occurrence: inRow,
        line: hit.line,
        sectionKey: section.key,
        rowFrom: from,
      });
      inRow += 1;
    }
  }
  return out;
}

/** The open job's log as the view shows it. */
export interface CILogFindShown {
  readonly jobId: string;
  readonly version: number;
  readonly sections: readonly CILogSection[];
}

/** The hits of an answer read for job `askedJob`, mapped onto the log now
 * shown; null when that is another job or another version of the log. */
export function ciLogAnswerHits(search: CILogSearch, askedJob: string, shown: CILogFindShown | null, limit: number): CILogFindHit[] | null {
  if (!shown || shown.jobId !== askedJob || shown.version !== search.version) return null;
  return ciLogFindHits(search, shown.sections, limit);
}
