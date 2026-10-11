import { describe, expect, it } from 'vitest';
import type { CIJob, CIStep } from '../types/models';
import { ciSectionText, segmentCILog, type CILogSegments } from './ciLogSections';
import runLogs from './__fixtures__/githubRunLogs.json';

// GitHub ground truth: the run-logs zip of a real run holds each job's
// combined log and, beside it, one file per step. Splitting the combined
// log by the jobs API's step times must give each step exactly its
// file's lines. Times in the two files differ in the last digits, so
// lines compare by their text after the time.

interface FixtureJob {
  id: number;
  name: string;
  status: string;
  log: string;
  steps: CIStep[];
  stepFiles: Record<string, string>;
}

const jobs = (runLogs as unknown as { jobs: FixtureJob[] }).jobs;

function fixtureJob(name: string): FixtureJob {
  const job = jobs.find((candidate) => candidate.name.startsWith(name));
  if (!job) throw new Error(`no fixture job ${name}`);
  return job;
}

function ciJob(job: FixtureJob, steps: CIStep[] = job.steps): CIJob {
  return { id: String(job.id), name: job.name, status: job.status, logsAvailable: true, steps };
}

const afterTime = (line: string) => line.slice(29);

function stepLines(job: FixtureJob, number: number): string[] {
  const file = job.stepFiles[String(number)];
  if (file === undefined) return [];
  const lines = file.split('\n');
  if (lines.at(-1) === '') lines.pop();
  return lines.map(afterTime);
}

function sectionLines(segments: CILogSegments, key: string): string[] {
  const section = segments.sections.find((candidate) => candidate.key === key);
  if (!section) throw new Error(`no section ${key}`);
  return segments.lines.slice(section.start, section.end).map(afterTime);
}

describe('segmentCILog on GitHub logs', () => {
  for (const name of ['svelte-check', 'golangci-lint']) {
    it(`gives every step of ${name} exactly its own lines`, () => {
      const job = fixtureJob(name);
      const segments = segmentCILog(job.log, false, ciJob(job));
      for (const step of job.steps) {
        if (step.status === 'skipped') continue;
        expect(sectionLines(segments, `step:${step.number}`), `step ${step.number} ${step.name}`).toEqual(stepLines(job, step.number));
      }
      // Every line belongs to one step, in order.
      const covered = segments.sections.reduce((sum, section) => sum + (section.end - section.start), 0);
      expect(covered).toBe(segments.lines.length);
    });
  }

  it('splits steps that start in the same second as the failed one', () => {
    // Steps 8, 14 and 15 all start at 00:04:31; step 8 failed.
    const job = fixtureJob('svelte-check');
    const segments = segmentCILog(job.log, false, ciJob(job));
    const failed = segments.sections.find((section) => section.key === 'step:8')!;
    expect(failed.status).toBe('failed');
    expect(sectionLines(segments, 'step:8').at(-1)).toBe('##[error]Process completed with exit code 2.');
    expect(sectionLines(segments, 'step:14')).toEqual(['Post job cleanup.']);
  });

  it('omits a skipped step and gives it no lines, though it has times', () => {
    // golangci's step 7 is skipped at 00:05:29, the second steps 12 to 14
    // start in.
    const job = fixtureJob('golangci-lint');
    const segments = segmentCILog(job.log, false, ciJob(job));
    expect(segments.sections.map((section) => section.key)).not.toContain('step:7');
    expect(sectionLines(segments, 'step:6').at(-1)).toBe('##[error]Process completed with exit code 2.');
    expect(sectionLines(segments, 'step:12')).toEqual(['Post job cleanup.']);
  });

  it('carries each step status and its duration from the step times', () => {
    const job = fixtureJob('svelte-check');
    const segments = segmentCILog(job.log, false, ciJob(job));
    const build = segments.sections.find((section) => section.key === 'step:6')!;
    expect(build).toMatchObject({ name: 'Run pnpm --dir frontend run build', status: 'success', durationSeconds: 9, truncatedTop: false });
  });

  it('lists steps the job has not reached as pending rows without lines', () => {
    const job = fixtureJob('svelte-check');
    // The job as it ran step 5: later steps have not started.
    const cut = job.steps.findIndex((step) => step.number === 6);
    const steps = job.steps.map((step, index): CIStep => {
      if (index < cut - 1) return step;
      if (index === cut - 1) return { ...step, status: 'running', completedAt: undefined };
      return { number: step.number, name: step.name, status: 'pending' };
    });
    const lines = job.log.split('\n');
    const end = lines.findIndex((line) => afterTime(line) === '##[group]Run pnpm --dir frontend run build');
    const text = lines.slice(0, end).join('\n') + '\n';
    const segments = segmentCILog(text, false, { ...ciJob(job, steps), status: 'running' });
    expect(sectionLines(segments, 'step:5')).toEqual(stepLines(job, 5));
    const running = segments.sections.find((section) => section.key === 'step:5')!;
    expect(running.status).toBe('running');
    expect(running.durationSeconds).toBeUndefined();
    const later = segments.sections.filter((section) => section.status === 'pending');
    expect(later.map((section) => section.key)).toEqual(['step:6', 'step:7', 'step:8', 'step:14', 'step:15', 'step:16', 'step:17']);
    for (const section of later) expect(section.end - section.start).toBe(0);
  });

  it('lists every step without lines while the log is not available', () => {
    const job = fixtureJob('svelte-check');
    const segments = segmentCILog('', false, ciJob(job));
    expect(segments.sections).toHaveLength(job.steps.length);
    for (const section of segments.sections) expect(section.end - section.start).toBe(0);
  });

  it('marks the step a tail cut starts in, and gives the steps above it no lines', () => {
    const job = fixtureJob('svelte-check');
    const lines = job.log.split('\n');
    lines.pop();
    // Cut inside step 4, past its first line.
    const step4 = stepLines(job, 4);
    const from = lines.findIndex((line) => afterTime(line) === step4[5]);
    const segments = segmentCILog(lines.slice(from).join('\n') + '\n', true, ciJob(job));
    for (const number of [1, 2, 3]) {
      const section = segments.sections.find((candidate) => candidate.key === `step:${number}`)!;
      expect(section.end - section.start, `step ${number}`).toBe(0);
    }
    expect(sectionLines(segments, 'step:4')).toEqual(step4.slice(5));
    expect(segments.sections.find((section) => section.key === 'step:4')!.truncatedTop).toBe(true);
    expect(segments.sections.filter((section) => section.truncatedTop)).toHaveLength(1);
    expect(sectionLines(segments, 'step:5')).toEqual(stepLines(job, 5));
  });

  it('does not mark a step the tail cut starts exactly at', () => {
    const job = fixtureJob('svelte-check');
    const lines = job.log.split('\n');
    lines.pop();
    const from = lines.findIndex((line) => afterTime(line) === '##[group]Run set -euo pipefail');
    const text = lines.slice(from).join('\n') + '\n';
    const segments = segmentCILog(text, true, ciJob(job));
    expect(sectionLines(segments, 'step:8')).toEqual(stepLines(job, 8));
    // Step 7 started in an earlier second, so the cut could have been in
    // it; the step-start line gives line 0 to step 8.
    const step7 = segments.sections.find((section) => section.key === 'step:7')!;
    expect(step7.end - step7.start).toBe(0);
    expect(segments.sections.reduce((sum, section) => sum + (section.end - section.start), 0)).toBe(segments.lines.length);
    expect(segments.sections.some((section) => section.truncatedTop)).toBe(false);

    // A step whose first line is in a later second than its start: the
    // cut at that line holds the step's own start.
    const early = job.steps.map((step) => (step.number === 8 ? { ...step, startedAt: '2026-10-11T00:04:30Z' } : step));
    const shifted = segmentCILog(text, true, ciJob(job, early));
    expect(sectionLines(shifted, 'step:8')).toEqual(stepLines(job, 8));
    expect(shifted.sections.some((section) => section.truncatedTop)).toBe(false);
  });

  it('shows the whole log as the job when it has no steps', () => {
    const segments = segmentCILog('a\nb\n', false, { id: '1', name: 'build', status: 'failed', logsAvailable: true });
    expect(segments.sections).toEqual([{ key: 'job', name: 'build', status: 'failed', start: 0, end: 2, truncatedTop: false }]);
  });

  it('keeps the lines as the job above steps without start times', () => {
    const job: CIJob = {
      id: '1',
      name: 'build',
      status: 'running',
      logsAvailable: true,
      steps: [{ number: 1, name: 'Set up job', status: 'running' }, { number: 2, name: 'Test', status: 'pending' }],
    };
    const segments = segmentCILog('a\nb\n', false, job);
    expect(segments.sections.map(({ key, start, end }) => ({ key, start, end }))).toEqual([
      { key: 'job', start: 0, end: 2 },
      { key: 'step:1', start: 0, end: 0 },
      { key: 'step:2', start: 0, end: 0 },
    ]);
  });
});

const GITLAB_JOB: CIJob = { id: '9', name: 'unit', status: 'failed', logsAvailable: true };

// A cleaned GitLab trace, as internal/git cleanGitLabTrace leaves it.
const TRACE = [
  'Running with gitlab-runner 17.0.0',
  'section_start:1700000000:prepare_executor',
  '\x1b[36;1mPreparing the "docker" executor\x1b[0;m',
  'Using docker image alpine',
  'section_end:1700000004:prepare_executor',
  'section_start:1700000004:step_script',
  '2026-10-11T00:00:04.000000Z \x1b[36;1mExecuting "step_script" stage of the job script\x1b[0;m',
  '$ make test',
  'section_start:1700000005:custom_inner[collapsed=true]',
  'inner header',
  'inner line',
  'section_end:1700000006:custom_inner',
  'FAIL',
  'section_end:1700000070:step_script',
  '\x1b[31;1mERROR: Job failed: exit code 1\x1b[0;m',
  '',
].join('\n');

describe('segmentCILog on GitLab traces', () => {
  it('makes a row of each top-level section and of the output between them', () => {
    const segments = segmentCILog(TRACE, false, GITLAB_JOB);
    expect(segments.lines.some((line) => line.startsWith('section_'))).toBe(false);
    expect(segments.sections.map(({ key, name, status, durationSeconds }) => ({ key, name, status, durationSeconds }))).toEqual([
      { key: 'output:start', name: 'Running with gitlab-runner 17.0.0', status: 'output', durationSeconds: undefined },
      { key: 'section:prepare_executor:1700000000', name: 'Preparing the "docker" executor', status: 'done', durationSeconds: 4 },
      { key: 'section:step_script:1700000004', name: 'Executing "step_script" stage of the job script', status: 'done', durationSeconds: 66 },
      { key: 'output:section:step_script:1700000004', name: 'ERROR: Job failed: exit code 1', status: 'output', durationSeconds: undefined },
    ]);
    // A nested section stays in its parent's text, without its markers.
    const script = segments.sections[2]!;
    expect(ciSectionText(segments, script)).toBe(
      '2026-10-11T00:00:04.000000Z Executing "step_script" stage of the job script\n$ make test\ninner header\ninner line\nFAIL',
    );
  });

  it('shows the section a running job is in as running, with its lines so far', () => {
    const running = TRACE.split('\n').slice(0, 11).join('\n') + '\n';
    const segments = segmentCILog(running, false, { ...GITLAB_JOB, status: 'running' });
    const last = segments.sections.at(-1)!;
    expect(last).toMatchObject({ key: 'section:step_script:1700000004', status: 'running' });
    expect(last.durationSeconds).toBeUndefined();
    expect(segments.lines.slice(last.start, last.end).at(-1)).toBe('inner line');
  });

  it('keeps a row key as the trace grows', () => {
    const lines = TRACE.split('\n');
    const early = segmentCILog(lines.slice(0, 8).join('\n'), false, { ...GITLAB_JOB, status: 'running' });
    const late = segmentCILog(TRACE, false, GITLAB_JOB);
    expect(early.sections.map((section) => section.key)).toEqual(late.sections.slice(0, 3).map((section) => section.key));
  });

  it('marks the section a tail cut opens in, by the end marker it still has', () => {
    const tail = TRACE.split('\n').slice(7).join('\n');
    const segments = segmentCILog(tail, true, GITLAB_JOB);
    expect(segments.sections[0]).toMatchObject({ key: 'section:step_script:cut', name: 'step_script', truncatedTop: true });
    expect(ciSectionText(segments, segments.sections[0]!)).toBe('$ make test\ninner header\ninner line\nFAIL');
    expect(segments.sections.filter((section) => section.truncatedTop)).toHaveLength(1);
  });

  it('takes a cut row over by the outer section a cut nested section sits in', () => {
    const tail = TRACE.split('\n').slice(10).join('\n');
    const segments = segmentCILog(tail, true, GITLAB_JOB);
    expect(segments.sections[0]).toMatchObject({ key: 'section:step_script:cut', truncatedTop: true });
    expect(ciSectionText(segments, segments.sections[0]!)).toBe('inner line\nFAIL');
  });

  it('shows a trace without sections as the job', () => {
    const segments = segmentCILog('line 1\nline 2\n', false, { ...GITLAB_JOB, status: 'running' });
    expect(segments.sections).toEqual([{ key: 'job', name: 'unit', status: 'running', start: 0, end: 2, truncatedTop: false }]);
  });
});
