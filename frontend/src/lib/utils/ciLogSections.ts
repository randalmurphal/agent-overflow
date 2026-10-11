import type { CIJob, CIStep } from '../types/models';

// Splits a CI job's log text into the rows the log view shows: GitHub
// steps and GitLab sections.
//
// GitHub: every log line starts with its own time to 100 ns
// (`2026-10-11T00:04:31.5805438Z text`), while the jobs API gives each
// step's start floored to the second, and several steps often start in
// the same second. A step's first line is at or after its start second,
// so the time bounds where it can begin; inside the start second a
// line the runner writes when a step begins decides: first the one this
// step's name implies (stepOwnLine), then any of STEP_START_LINE. Skipped steps and steps not
// reached write no lines and take no part in the split.
//
// GitLab: the cleaned trace (internal/git cleanGitLabTrace) holds each
// `section_start:<unix>:<name>` and `section_end:<unix>:<name>` marker
// on a line of its own, and the line after a start is the section's
// header. Top-level sections are rows; nested sections stay inside
// their parent's text. Lines outside every section form rows of their
// own. Markers are never part of any row's text.
//
// The text can be the tail of a longer log (the backend's display cap),
// cut at a line boundary. A row whose start the cut removed is marked
// truncatedTop.

export interface CILogSection {
  /** Stable for the job while its log grows or its tail window slides. */
  key: string;
  name: string;
  /** A CI status (success, failed, running, pending, canceled, ...), or
   * `done` for a closed GitLab section (GitLab reports no per-section
   * result) and `output` for lines outside every section. */
  status: string;
  durationSeconds?: number;
  /** Line range in CILogSegments.lines, end exclusive. */
  start: number;
  end: number;
  /** The text starts partway through this row. */
  truncatedTop: boolean;
}

export interface CILogSegments {
  /** The log's lines, markers removed. */
  lines: string[];
  sections: CILogSection[];
}

export const CI_SECTION_DONE = 'done';
export const CI_SECTION_OUTPUT = 'output';

// A GitHub line's time prefix is 28 characters and a space.
const GITHUB_PREFIX = 29;
const GITHUB_TIME = /^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}\.\d+Z $/;

// Lines the runner writes as a step's first line: a run step's group, a
// composite action's preparation, a post step, container setup and
// teardown, job completion.
const STEP_START_LINE =
  /^(?:##\[group\]Run |##\[group\]Checking docker version$|Prepare all required actions$|Post job cleanup\.$|Stop and remove container: |Cleaning up orphan processes$)/;

/** The first line the runner writes for a step of this name, where the
 * name decides it. `Set up job` also prepares actions, so a composite
 * step's `Prepare all required actions` is not one of these. */
function stepOwnLine(name: string): (body: string) => boolean {
  switch (name) {
    case 'Initialize containers':
      return (body) => body === '##[group]Checking docker version';
    case 'Stop containers':
      return (body) => body.startsWith('Stop and remove container: ');
    case 'Complete job':
      return (body) => body === 'Cleaning up orphan processes';
  }
  if (name.startsWith('Post ')) return (body) => body === 'Post job cleanup.';
  const group = `##[group]${name}`;
  return (body) => body === group;
}

const GITLAB_MARKER = /^section_(start|end):(\d+):([A-Za-z0-9_.-]+)(?:\[[^\]]*\])?$/;
const LEADING_TIME = /^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d+)?Z /;
// SGR and erase sequences (ESC is the one control character that opens
// one); the trace cleaner already resolved overwrites.
const ANSI_SEQUENCE = /\p{Cc}\[[0-9;?]*[A-Za-z]/gu;

export function segmentCILog(text: string, truncated: boolean, job: CIJob): CILogSegments {
  const raw = text ? text.split('\n') : [];
  if (raw.at(-1) === '') raw.pop();
  const steps = job.steps ?? [];
  if (steps.length > 0) return { lines: raw, sections: githubSections(raw, truncated, steps, job) };
  return gitlabSections(raw, truncated, job);
}

/** A row's text as plain lines: escape sequences removed, for the
 * clipboard and the chat. */
export function ciSectionText(segments: CILogSegments, section: CILogSection): string {
  return segments.lines.slice(section.start, section.end).join('\n').replace(ANSI_SEQUENCE, '');
}

/** `name` without a leading time or escape sequences. */
function plainName(line: string): string {
  return line.replace(LEADING_TIME, '').replace(ANSI_SEQUENCE, '').trim();
}

function secondsBetween(startedAt?: string, completedAt?: string): number | undefined {
  if (!startedAt || !completedAt) return undefined;
  const ms = Date.parse(completedAt) - Date.parse(startedAt);
  return Number.isFinite(ms) && ms >= 0 ? ms / 1000 : undefined;
}

/** `YYYY-MM-DDTHH:MM:SS` of a UTC time, comparable as a string with a
 * GitHub line's prefix. */
function utcSecond(time: string): string | null {
  const ms = Date.parse(time);
  return Number.isFinite(ms) ? new Date(ms).toISOString().slice(0, 19) : null;
}

function githubSections(lines: string[], truncated: boolean, steps: CIStep[], job: CIJob): CILogSection[] {
  // Each line's second; a line without its own time (a cut first line,
  // a blank) takes the one before it, or the first one after it.
  const second: string[] = Array.from({ length: lines.length }, () => '');
  let last = '';
  for (let i = 0; i < lines.length; i += 1) {
    const line = lines[i]!;
    if (line.length > GITHUB_PREFIX && GITHUB_TIME.test(line.slice(0, GITHUB_PREFIX))) last = line.slice(0, 19);
    second[i] = last;
  }
  const firstTimed = second.findIndex((s) => s !== '');
  for (let i = 0; i < firstTimed; i += 1) second[i] = second[firstTimed]!;
  const body = (i: number) => {
    const line = lines[i]!;
    return line.length > GITHUB_PREFIX && GITHUB_TIME.test(line.slice(0, GITHUB_PREFIX)) ? line.slice(GITHUB_PREFIX) : line;
  };

  const ordered = [...steps].sort((a, b) => a.number - b.number);
  const logged = ordered.filter((step) => step.status !== 'skipped' && step.startedAt && utcSecond(step.startedAt) !== null);
  // Where each logged step's lines begin; null for a step the text does
  // not reach (cut off above, or not written yet).
  const bounds = new Map<number, number | null>();
  let cutStep: number | null = null;
  let lo = 0;
  let startAt = 0;
  if (lines.length > 0 && logged.length > 0) {
    if (truncated) {
      // The step holding line 0 is the last one that started in an
      // earlier second; every step before it ended above the cut.
      const t0 = second[0]!;
      let holder = 0;
      logged.forEach((step, index) => {
        if (utcSecond(step.startedAt!)! < t0) holder = index;
      });
      for (let i = 0; i < holder; i += 1) bounds.set(logged[i]!.number, null);
      bounds.set(logged[holder]!.number, 0);
      cutStep = logged[holder]!.number;
      startAt = holder + 1;
    } else {
      bounds.set(logged[0]!.number, 0);
      startAt = 1;
    }
    lo = 1;
  }
  for (let index = startAt; index < logged.length && lines.length > 0; index += 1) {
    const step = logged[index]!;
    const s = utcSecond(step.startedAt!)!;
    // The first line at or after the step's second.
    let at = lo;
    while (at < lines.length && second[at]! < s) at += 1;
    if (at >= lines.length) {
      bounds.set(step.number, null);
      continue;
    }
    // Inside the start second, up to the first line of a later second,
    // the step's own line wins, then any step-start line. Line 0 can be
    // the step's start in a cut text even though `lo` is past it.
    const from = cutStep !== null && index === startAt ? 0 : at;
    let end = at;
    while (end < lines.length && second[end]! === s) end += 1;
    const own = stepOwnLine(step.name);
    let pick = -1;
    for (let i = from; i <= end && i < lines.length; i += 1) {
      if (own(body(i))) { pick = i; break; }
    }
    if (pick < 0) {
      for (let i = from; i <= end && i < lines.length; i += 1) {
        if (STEP_START_LINE.test(body(i))) { pick = i; break; }
      }
    }
    const bound = pick >= 0 ? pick : at;
    bounds.set(step.number, bound);
    lo = bound + 1;
  }

  const sections: CILogSection[] = [];
  const starts = logged
    .map((step) => bounds.get(step.number))
    .filter((bound): bound is number => typeof bound === 'number');
  for (const step of ordered) {
    if (step.status === 'skipped') continue;
    const bound = bounds.get(step.number);
    let start = 0;
    let end = 0;
    if (typeof bound === 'number') {
      start = bound;
      end = starts.find((other) => other > bound) ?? lines.length;
    }
    sections.push({
      key: `step:${step.number}`,
      name: step.name,
      status: step.status,
      durationSeconds: secondsBetween(step.startedAt, step.completedAt),
      start,
      end,
      // The holder starts partway unless the cut fell on its first line.
      truncatedTop: step.number === cutStep && end > start && !STEP_START_LINE.test(body(0)),
    });
  }
  // A step claimed by a later one at the same line keeps no lines.
  for (let i = 0; i < sections.length; i += 1) {
    const section = sections[i]!;
    if (section.end <= section.start) continue;
    const later = sections.slice(i + 1).find((other) => other.end > other.start);
    if (later && later.start <= section.start) {
      section.end = section.start;
      section.truncatedTop = false;
    }
  }
  if (starts.length === 0 && lines.length > 0) {
    // No step has a start time the text can be split by: the lines are
    // the job's, above its step list.
    sections.unshift({ key: 'job', name: job.name, status: job.status, start: 0, end: lines.length, truncatedTop: truncated });
  }
  return sections;
}

function gitlabSections(raw: string[], truncated: boolean, job: CIJob): CILogSegments {
  const lines: string[] = [];
  const sections: CILogSection[] = [];
  // The open top-level row and the names opened inside it.
  let open: { name: string; unix: number; start: number; header: number } | null = null;
  const stack: string[] = [];
  let looseStart = 0;
  let previousKey = 'start';
  let sawMarker = false;
  const live = job.status === 'running' || job.status === 'pending';

  const closeLoose = (end: number) => {
    if (end <= looseStart) return;
    const first = lines.slice(looseStart, end).find((line) => plainName(line) !== '');
    if (first === undefined) return;
    const key = `output:${previousKey}`;
    sections.push({
      key,
      name: plainName(first),
      status: CI_SECTION_OUTPUT,
      start: looseStart,
      end,
      truncatedTop: truncated && looseStart === 0,
    });
    previousKey = key;
  };

  for (const line of raw) {
    const marker = GITLAB_MARKER.exec(line);
    if (!marker) {
      lines.push(line);
      continue;
    }
    sawMarker = true;
    const [, kind, unixText, name] = marker as unknown as [string, string, string, string];
    const unix = Number(unixText);
    if (kind === 'start') {
      if (open) {
        stack.push(name);
        continue;
      }
      closeLoose(lines.length);
      open = { name, unix, start: lines.length, header: lines.length };
      stack.length = 0;
      stack.push(name);
      continue;
    }
    // section_end
    const depth = stack.lastIndexOf(name);
    if (open && depth > 0) {
      stack.length = depth;
      continue;
    }
    if (open && depth === 0) {
      const headerLine = lines[open.header];
      const key = `section:${open.name}:${open.unix}`;
      sections.push({
        key,
        name: (open.header < lines.length && plainName(headerLine!)) || open.name,
        status: CI_SECTION_DONE,
        durationSeconds: unix >= open.unix ? unix - open.unix : undefined,
        start: open.start,
        end: lines.length,
        truncatedTop: false,
      });
      previousKey = key;
      open = null;
      stack.length = 0;
      looseStart = lines.length;
      continue;
    }
    if (!open && truncated) {
      // The cut removed this section's start. Sections nest, so every
      // row above, nested sections included, is its tail; an outer
      // section the cut also opened ends later and takes the row over.
      const key = `section:${name}:cut`;
      sections.length = 0;
      sections.push({ key, name, status: CI_SECTION_DONE, start: 0, end: lines.length, truncatedTop: true });
      previousKey = key;
      looseStart = lines.length;
    }
    // Any other end without its start is ignored.
  }

  if (open) {
    const headerLine = lines[open.header];
    sections.push({
      key: `section:${open.name}:${open.unix}`,
      name: (open.header < lines.length && plainName(headerLine!)) || open.name,
      status: live ? 'running' : job.status,
      start: open.start,
      end: lines.length,
      truncatedTop: false,
    });
  } else if (!sawMarker) {
    if (lines.length > 0) {
      sections.push({ key: 'job', name: job.name, status: job.status, start: 0, end: lines.length, truncatedTop: truncated });
    }
  } else {
    closeLoose(lines.length);
  }
  return { lines, sections };
}
