// A project's failed read under its heading: the sentence, Retry where a
// retry can help, and Details with the correlation id and the forge's raw
// error, from the list snapshot's per-project detail or a failed call.

import { fireEvent, render, waitFor, within } from '@testing-library/svelte';
import { afterEach, describe, expect, it } from 'vitest';
import PullsProjectError from './PullsProjectError.svelte';
import { projectErrors } from '../../stores/pullsPageRows';
import { HOME_BACKEND } from '../../transport/backendKey';
import { TransportError } from '../../transport/wsClient';
import { setBindingMock } from '../../../test/mocks/bindings-app';
import { pullsProject, pullsSnapshot } from '../../../test/helpers/pullsPane';

const REFUSED = pullsProject([], {
  error: 'Your GitHub token lacks the `repo` scope, so the app cannot read owner/repo.',
  errorKind: 'forbidden',
  errorDetails: { ref: 'cid-1', method: 'repository read', at: 1_700_000_000_000, chain: ['forge refused (token lacks the repo scope): forge GET https://api.github.com/search: HTTP 403'] },
});

afterEach(() => {
  document.body.replaceChildren();
});

describe('PullsProjectError', () => {
  it("shows the list's correlation id and the forge's text behind Details", async () => {
    const logs = setBindingMock('GetErrorLogLines', async () => ({ lines: ['pr list: read of github:owner/repo failed (id: cid-1)'], found: true }));
    const [failed] = projectErrors(pullsSnapshot([REFUSED]), [], HOME_BACKEND);
    const view = render(PullsProjectError, { ...failed!, onRetry: () => {} });

    expect(view.getByTestId('pulls-project-error')).toHaveTextContent('lacks the `repo` scope');
    expect(view.queryByTestId('pulls-project-retry')).toBeNull();
    await fireEvent.click(view.getByTestId('error-details-toggle'));
    const details = view.getByTestId('error-details');
    expect(details).toHaveTextContent('ref cid-1');
    expect(details).toHaveTextContent('HTTP 403');
    await waitFor(() => expect(logs).toHaveBeenCalledWith('cid-1'));
  });

  it('captures a failure once while the snapshots repeat it', async () => {
    const logs = setBindingMock('GetErrorLogLines', async () => ({ lines: [], found: false }));
    const first = projectErrors(pullsSnapshot([REFUSED]), [], HOME_BACKEND)[0]!;
    const view = render(PullsProjectError, { ...first, onRetry: () => {} });
    await waitFor(() => expect(logs).toHaveBeenCalledTimes(1));
    const repeat = projectErrors(pullsSnapshot([REFUSED]), [], HOME_BACKEND)[0]!;
    expect(repeat.cause).not.toBe(first.cause);
    await view.rerender({ ...repeat, onRetry: () => {} });
    const next = projectErrors(pullsSnapshot([pullsProject([], { ...REFUSED, errorDetails: { ...REFUSED.errorDetails!, ref: 'cid-2' } })]), [], HOME_BACKEND)[0]!;
    await view.rerender({ ...next, onRetry: () => {} });
    await waitFor(() => expect(logs).toHaveBeenCalledTimes(2));
    expect(logs).toHaveBeenLastCalledWith('cid-2');
  });

  it("shows a failed call's details, and none without a cause", async () => {
    setBindingMock('GetErrorLogLines', async () => ({ lines: [], found: false }));
    const cause = new TransportError('forge_transient', 'gitlab.com could not be reached.', {
      detail: { ref: 'cid-9', method: 'ListProjectPipelines', at: 1, chain: ['gitlab.com could not be reached.', 'forge unreachable: dial tcp: i/o timeout'] },
    });
    const view = render(PullsProjectError, { message: cause.message, failure: { kind: 'transient' } as never, forge: 'gitlab', cause, onRetry: () => {} });
    expect(view.getByTestId('pulls-project-retry')).toBeInTheDocument();
    await fireEvent.click(view.getByTestId('error-details-toggle'));
    expect(within(view.getByTestId('error-details')).getByText(/i\/o timeout/)).toBeInTheDocument();

    view.unmount();
    const bare = render(PullsProjectError, { message: 'origin unreadable', failure: null, forge: 'github', onRetry: () => {} });
    expect(bare.queryByTestId('error-details-toggle')).toBeNull();
  });
});
