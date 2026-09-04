// <RenderBoundary> is the one containment wrapper for a render throw. An
// uncaught throw inside an update flush aborts the whole batch, so every
// region the traversal had not reached keeps its stale DOM for good (a
// composer that will not clear, a reveal stopped mid-message — incidents
// 2026-08-29 and 2026-09-04). Inside the boundary the throw tears down only
// its own subtree, which renders the failure with a Retry, and the boundary
// writes the record `window.onerror` no longer gets to see.
//
// The diagnostic is asserted through the REAL frontendErrorCapture pipeline
// (`setBindingMock('ReportFrontendErrorBatch', …)`, not a module mock):
// `vi.mock` does not reliably reach `.svelte.ts` importers, so a mocked
// capture module could silently leave the component on the real one.
import { fireEvent, render, waitFor } from '@testing-library/svelte';
import { beforeEach, describe, expect, it } from 'vitest';
import Harness from '../../../test/fixtures/RenderBoundaryHarness.svelte';
import { resetBindingMocks, setBindingMock } from '../../../test/mocks/bindings-app';
import {
  frontendErrorCaptureStateForTest,
  resetFrontendErrorCaptureForTest,
} from '../../utils/frontendErrorCapture';

beforeEach(() => {
  resetBindingMocks();
  resetFrontendErrorCaptureForTest();
  setBindingMock('ReportFrontendErrorBatch', async () => '/tmp/frontend-errors.jsonl');
});

describe('<RenderBoundary>', () => {
  it('renders its children and records nothing when nothing throws', () => {
    const view = render(Harness, { props: { child: 'ok' } });

    expect(view.getByTestId('boundary-child').textContent).toBe('child content');
    expect(view.queryByTestId('boundary-render-error')).toBeNull();
    expect(frontendErrorCaptureStateForTest().pendingCount).toBe(0);
  });

  it('shows the labelled failure row with a Retry and records one diagnostic', async () => {
    const view = render(Harness, {
      props: { child: 'throws', label: 'This thread pane', testId: 'pane-render-error' },
    });

    const alert = await waitFor(() => view.getByTestId('pane-render-error'));
    // Label plus the thrown message: the row has to say WHICH region died
    // and why, because the whole point is that the failure is visible in
    // place instead of a silent stale region.
    expect(alert.textContent).toContain('This thread pane failed to render: fixture render failure');
    expect(view.getByTestId('pane-render-error-retry')).toBeTruthy();
    expect(view.queryByTestId('boundary-child')).toBeNull();
    // On disk as well as on screen — the boundary swallowed the throw
    // before window.onerror could log it.
    expect(frontendErrorCaptureStateForTest().pendingCount).toBe(1);
  });

  it('Retry re-renders the children and drops the failure row', async () => {
    let willThrow = true;
    const view = render(Harness, {
      props: {
        child: 'gated',
        label: 'The thread list',
        testId: 'sidebar-render-error',
        shouldThrow: () => willThrow,
      },
    });

    const alert = await waitFor(() => view.getByTestId('sidebar-render-error'));
    expect(alert.textContent).toContain('The thread list failed to render: gated render failure');

    // Whatever made the child throw is gone — a row that has since loaded,
    // a list that no longer collides. Retry must get the user back to the
    // real surface, not a second failure row.
    willThrow = false;
    await fireEvent.click(view.getByTestId('sidebar-render-error-retry'));

    await waitFor(() => expect(view.getByTestId('boundary-child').textContent).toBe('child content'));
    expect(view.queryByTestId('sidebar-render-error')).toBeNull();
    // The one throw that happened is still the one record: a successful
    // retry does not re-report, and does not erase what failed either.
    expect(frontendErrorCaptureStateForTest().pendingCount).toBe(1);
  });
});
