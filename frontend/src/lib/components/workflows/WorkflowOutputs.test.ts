import { fireEvent, render } from '@testing-library/svelte';
import { describe, expect, it, vi } from 'vitest';
import type { WorkflowArtifact } from '../../../../bindings/agent-overflow/models';
import WorkflowOutputs from './WorkflowOutputs.svelte';

const artifacts = [{ name: 'report', path: '/tmp/run/report.pdf', size: 12 }] as WorkflowArtifact[];

describe('WorkflowOutputs', () => {
  it('renders named values before artifacts and opens an artifact through the host opener', async () => {
    const onOpenArtifact = vi.fn();
    const view = render(WorkflowOutputs, {
      values: { summary: 'All checks passed' },
      artifacts,
      viewOnly: false,
      onOpenArtifact,
    });

    expect(view.getByTestId('wf-output-values')).toHaveTextContent('summary');
    expect(view.getByTestId('wf-output-values')).toHaveTextContent('All checks passed');
    await fireEvent.click(view.getByTestId('wf-output-file'));
    expect(onOpenArtifact).toHaveBeenCalledWith('/tmp/run/report.pdf');
  });

  it('disables artifact rows remotely with a Local only title', () => {
    const view = render(WorkflowOutputs, {
      values: {},
      artifacts,
      viewOnly: true,
      onOpenArtifact: vi.fn(),
    });

    const artifact = view.getByTestId('wf-output-file') as HTMLButtonElement;
    expect(artifact.disabled).toBe(true);
    expect(artifact.title).toBe('Local only');
  });

  it('renders nothing when the run declared no outputs', () => {
    const view = render(WorkflowOutputs, {
      values: {},
      artifacts: [],
      viewOnly: false,
      onOpenArtifact: vi.fn(),
    });

    expect(view.queryByTestId('wf-outputs')).not.toBeInTheDocument();
  });
});
