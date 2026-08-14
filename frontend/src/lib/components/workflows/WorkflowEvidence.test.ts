import { render } from '@testing-library/svelte';
import { describe, expect, it } from 'vitest';
import type { WorkflowItemDetail, WorkItem } from '../../types/workflow';
import WorkflowEvidence from './WorkflowEvidence.svelte';

function detail(overrides: Partial<WorkflowItemDetail> = {}): WorkflowItemDetail {
  return {
    item: {
      id: 'run', projectId: 'p', workflowId: 'wf', goal: 'Run',
      state: 'needs-human', reason: 'setup-failed', sortPosition: 0, createdAt: 1,
    },
    phases: [],
    checkPhaseIds: [],
    callPhaseIds: [],
    units: [],
    outputs: {},
    artifacts: [],
    usage: { inputTokens: 0, outputTokens: 0, totalTokens: 0, costUsd: 0 },
    ...overrides,
  } as unknown as WorkflowItemDetail;
}

function props(view: WorkflowItemDetail) {
  return { item: view.item as unknown as WorkItem, detail: view, kind: 'blocked' as const, failedUnit: null, expandFirstDiff: false };
}

describe('WorkflowEvidence park cause', () => {
  // An engine-diagnosed park ran no turn, so every other evidence block on the
  // page has nothing to show for it. Before the cause was persisted, this state
  // rendered as a parked attempt and no explanation anywhere in the app.
  it('renders the cause of the attempt the run is resting on', () => {
    const view = render(WorkflowEvidence, props(detail({
      phases: [
        { phaseId: 'plan', attempt: 1, status: 'completed', startedAt: 1, endedAt: 2 },
        { phaseId: 'implement', attempt: 1, status: 'parked', startedAt: 3, endedAt: 3, cause: 'provision worktree: branch "ao/wave-3" already exists' },
      ],
    } as unknown as Partial<WorkflowItemDetail>)));

    expect(view.getByTestId('workflow-park-cause'))
      .toHaveTextContent('provision worktree: branch "ao/wave-3" already exists');
  });

  it('renders nothing when no attempt carries an engine-diagnosed cause', () => {
    const view = render(WorkflowEvidence, props(detail({
      phases: [
        { phaseId: 'ask', attempt: 1, status: 'parked', startedAt: 1, endedAt: 2, outputEnvelope: JSON.stringify({ status: 'question', question: 'which base branch?' }) },
      ],
    } as unknown as Partial<WorkflowItemDetail>)));

    expect(view.queryByTestId('workflow-park-cause')).toBeNull();
  });

  // A `--phase` repair leaves the old parked row behind with its cause intact.
  // Only the attempt the run is resting on may supply the diagnosis — scanning
  // back for any attempt with a cause resurrected the repaired park.
  it('does not resurrect an earlier repaired park as the current diagnosis', () => {
    const view = render(WorkflowEvidence, props(detail({
      phases: [
        { phaseId: 'plan', attempt: 1, status: 'parked', startedAt: 1, endedAt: 2, cause: 'provision worktree: branch "ao/wave-3" already exists' },
        { phaseId: 'review', attempt: 1, status: 'parked', startedAt: 3, endedAt: 4, outputEnvelope: JSON.stringify({ status: 'question', question: 'merge order?' }) },
      ],
    } as unknown as Partial<WorkflowItemDetail>)));

    expect(view.queryByTestId('workflow-park-cause')).toBeNull();
  });
});

// R1: the surface has two hues and green is not one of them. A passing check
// is the expected outcome and carries no attention, so it reads neutral —
// same glyph and tone the run map's node for that phase draws.
describe('WorkflowEvidence check strip speaks the run map\'s vocabulary', () => {
  function checks() {
    const view = detail({
      checkPhaseIds: ['lint', 'types', 'e2e'],
      phases: [
        { phaseId: 'lint', attempt: 1, status: 'completed', startedAt: 1, endedAt: 2 },
        { phaseId: 'types', attempt: 1, status: 'failed', startedAt: 1, endedAt: 2 },
        { phaseId: 'e2e', attempt: 1, status: 'running', startedAt: 1, endedAt: 0 },
      ],
    } as unknown as Partial<WorkflowItemDetail>);
    // The strip is suppressed for an unresolved run, so this is a `done` one.
    view.item.state = 'done';
    view.item.reason = undefined as unknown as string;
    return render(WorkflowEvidence, { ...props(view), kind: 'done' as const });
  }

  it('gives failure the only hue and leaves the rest neutral', () => {
    const rendered = checks();
    const rows = rendered.getAllByTestId('workflow-check');
    expect(rows.map((row) => row.className)).toEqual(['text-fg-muted', 'text-error', 'text-fg-muted']);
    expect(rendered.container.querySelector('.text-success')).toBeNull();
  });

  it('draws the map\'s glyphs rather than a set of its own', () => {
    const rows = checks().getAllByTestId('workflow-check');
    expect(rows.map((row) => row.textContent?.trim().charAt(0))).toEqual(['✓', '✗', '◌']);
  });

  // The wire status is on the row so the hue and the glyph above can be read
  // against WHICH check they describe. Without it both assertions are about an
  // ordering the fixture happens to have, and a reordered projection would keep
  // them both passing while the strip said something else.
  it('carries each check\'s wire status on the row it styled', () => {
    const rows = checks().getAllByTestId('workflow-check');
    expect(rows.map((row) => row.dataset.checkStatus)).toEqual(['completed', 'failed', 'running']);
  });
});

describe('WorkflowEvidence exhausted limits', () => {
  it('names provider retry exhaustion and keeps the failure evidence', () => {
    const view = detail({ phases: [] });
    view.item.reason = 'provider-retries-exhausted';
    const rendered = render(WorkflowEvidence, { ...props(view), kind: 'paused' as const });

    expect(rendered.getByTestId('workflow-paused-receipt')).toHaveTextContent('provider retries exhausted');
    expect(rendered.getByTestId('workflow-paused-receipt')).not.toHaveTextContent('paused by you');
    expect(rendered.getByTestId('wf-failure-evidence')).toBeTruthy();
  });

  it('explains that a usage-limited resume is an immediate real retry', () => {
    const view = detail({ phases: [] });
    view.item.reason = 'provider-usage-limited';
    const rendered = render(WorkflowEvidence, { ...props(view), kind: 'paused' as const });

    expect(rendered.getByTestId('workflow-paused-receipt')).toHaveTextContent('provider usage limit reached');
    expect(rendered.getByTestId('workflow-paused-receipt')).toHaveTextContent('resume attempts again immediately');
    expect(rendered.getByTestId('wf-failure-evidence')).toBeTruthy();
  });

  it('tells a spent workflow loop from a provider retry failure', () => {
    const view = detail({ phases: [] });
    view.item.reason = 'loop-limit-exhausted';
    const rendered = render(WorkflowEvidence, { ...props(view), kind: 'blocked' as const });

    expect(rendered.getByTestId('workflow-paused-receipt')).toHaveTextContent('workflow loop limit exhausted');
    expect(rendered.getByTestId('workflow-paused-receipt')).toHaveTextContent('restart from an earlier phase');
    expect(rendered.getByTestId('wf-failure-evidence')).toBeTruthy();
  });

  it('does not invent the cause for a legacy retry park', () => {
    const view = detail({ phases: [] });
    view.item.reason = 'retries-exhausted';
    const rendered = render(WorkflowEvidence, { ...props(view), kind: 'paused' as const });

    expect(rendered.getByTestId('workflow-paused-receipt')).toHaveTextContent('retry or loop limit exhausted');
  });

  it('still reads a genuine pause as one', () => {
    const view = detail({ phases: [] });
    view.item.reason = 'paused';
    const rendered = render(WorkflowEvidence, { ...props(view), kind: 'paused' as const });

    expect(rendered.getByTestId('workflow-paused-receipt')).toHaveTextContent('paused by you');
    expect(rendered.queryByTestId('wf-failure-evidence')).toBeNull();
  });
});
