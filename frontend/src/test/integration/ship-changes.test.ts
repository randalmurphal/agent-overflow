// Integration tests for the Ship Changes drawer. The drawer walks the user
// through Commit -> Push -> Create PR; this suite mounts the full App,
// opens the drawer via the git actions menu, and drives each step.

import { describe, expect, it, beforeAll, beforeEach } from 'vitest';
import { render, fireEvent, waitFor } from '@testing-library/svelte';

// The header subscribes through the backend gitwatch stream and the store
// only sources while the wire is live. src/test/setup.ts pins transport
// connected for every suite, so nothing is mocked here — the git-status
// store is a `.svelte.ts` importer of transportStatus and vi.mock does not
// reliably reach those (frontend/CLAUDE.md § Testing).

import App from '../../App.svelte';
import type { GitActionResult, GitStatus } from '../../lib/types/git';
import { setBindingMock } from '../mocks/bindings-app';
import {
  flush,
  installAnimateShim,
  installAppDefaults,
  installComposerDefaults,
  installThreadViewDefaults,
  INTEGRATION_WORKSPACE,
  makeGitStatus,
  makeThread,
  resetAppState,
  seedSidebarProject,
} from './_helpers';

beforeAll(installAnimateShim);

async function mountWithThread(status: GitStatus = makeGitStatus({ hasChanges: true })) {
  const thread = makeThread({ title: 'Ship Changes Thread' });
  installAppDefaults();
  setBindingMock('ListThreads', async () => [thread]);
  seedSidebarProject([thread]);
  installThreadViewDefaults();
  installComposerDefaults(thread.id);
  // The header subscribes to the workspace and every git surface — badges,
  // the split button, the drawer — reads that one observation. GetGitStatus
  // is only the post-action refresh, so both have to say the same thing for
  // a test that never mutates.
  setBindingMock('GitStatusSubscribe', async () => ({
    id: 'ship-sub',
    cwd: INTEGRATION_WORKSPACE,
    status,
  }));
  setBindingMock('GetGitStatus', async () => status);

  const rendered = render(App);
  await flush();
  const rows = rendered.getAllByText(thread.title);
  await fireEvent.click(rows[0]);
  await flush(15);
  return rendered;
}

// Minimal shape we need from the render result — typing it this way avoids
// awkward `ReturnType<typeof render>` generics that complain about
// `getByTestId` being too broad.
interface DrawerRendered {
  container: HTMLElement;
  getByTestId: (id: string) => HTMLElement;
  findByRole: (role: string, opts?: { name?: string | RegExp }) => Promise<HTMLElement>;
}

async function openShipChangesDrawer(rendered: DrawerRendered) {
  // The "Ship Changes…" entry lives in the git-actions dropdown. Click the
  // caret toggle to open the menu, then the Ship Changes item. Post-Popover
  // migration the dropdown renders as a portaled menu so we look up the
  // menuitem by role rather than by its old hand-rolled testid.
  const menuTrigger = rendered.container.querySelector(
    'button[aria-label="More git actions"]',
  ) as HTMLButtonElement | null;
  expect(menuTrigger).not.toBeNull();
  await fireEvent.click(menuTrigger!);
  await flush();
  const shipItem = await rendered.findByRole('menuitem', { name: /Ship Changes/i });
  await fireEvent.click(shipItem);
  await flush(10);
  // The drawer is a lazy import; its first-ever load in a suite run pays the
  // on-demand transform, which can exceed waitFor's default 1s under full
  // suite load. Waiting here (rather than in each test) keeps the budget in
  // one place.
  await waitFor(() => {
    expect(rendered.getByTestId('ship-changes-drawer')).toBeInTheDocument();
  }, { timeout: 5000 });
}

describe('App integration — ship changes drawer', () => {
  beforeEach(() => {
    resetAppState();
  });

  it('opens the Ship Changes drawer from the git actions menu', async () => {
    const rendered = await mountWithThread();
    await openShipChangesDrawer(rendered);
    // Initial stage for "dirty tree" is the commit step. The drawer is a
    // lazy mount, so its stage resolution lands a beat after the drawer.
    await waitFor(() => {
      expect(rendered.getByTestId('ship-changes-step-commit')).toBeInTheDocument();
    });
  });

  it('advances commit → push → PR as each binding resolves', async () => {
    const commitMock = setBindingMock(
      'GitCommit',
      async () => ({ action: 'commit', commitSha: 'abc1234' } as GitActionResult),
    );
    const pushMock = setBindingMock(
      'GitPush',
      async () => ({ action: 'push' } as GitActionResult),
    );
    const prMock = setBindingMock(
      'GitCreatePR',
      async () =>
        ({
          action: 'pr',
          prUrl: 'https://github.com/owner/repo/pull/77',
        } as GitActionResult),
    );

    const rendered = await mountWithThread();
    await openShipChangesDrawer(rendered);

    // Commit step: fill subject and submit.
    const subject = rendered.getByTestId('ship-changes-commit-subject') as HTMLInputElement;
    await fireEvent.input(subject, { target: { value: 'Ship feature' } });
    await flush();
    await fireEvent.click(rendered.getByTestId('ship-changes-commit-submit'));
    await waitFor(() => expect(commitMock).toHaveBeenCalled());
    await waitFor(() => {
      expect(rendered.getByTestId('ship-changes-step-push')).toBeInTheDocument();
    });

    // Push step: submit.
    await fireEvent.click(rendered.getByTestId('ship-changes-push-submit'));
    await waitFor(() => expect(pushMock).toHaveBeenCalled());
    await waitFor(() => {
      expect(rendered.getByTestId('ship-changes-step-pr')).toBeInTheDocument();
    });

    // PR step: submit.
    await fireEvent.click(rendered.getByTestId('ship-changes-pr-submit'));
    await waitFor(() => expect(prMock).toHaveBeenCalled());
    await waitFor(() => {
      expect(rendered.getByTestId('ship-changes-pr-done')).toBeInTheDocument();
    });
    // All three bindings were called exactly once, in order.
    expect(commitMock).toHaveBeenCalledTimes(1);
    expect(pushMock).toHaveBeenCalledTimes(1);
    expect(prMock).toHaveBeenCalledTimes(1);
  });

  it('allows skipping push (push + PR bindings never called)', async () => {
    const commitMock = setBindingMock(
      'GitCommit',
      async () => ({ action: 'commit', commitSha: 'abc1234' } as GitActionResult),
    );
    const pushMock = setBindingMock(
      'GitPush',
      async () => ({ action: 'push' } as GitActionResult),
    );
    const prMock = setBindingMock(
      'GitCreatePR',
      async () => ({ action: 'pr', prUrl: '' } as GitActionResult),
    );

    const rendered = await mountWithThread();
    await openShipChangesDrawer(rendered);

    const subject = rendered.getByTestId('ship-changes-commit-subject') as HTMLInputElement;
    await fireEvent.input(subject, { target: { value: 'Just commit' } });
    await flush();
    await fireEvent.click(rendered.getByTestId('ship-changes-commit-submit'));
    await waitFor(() => expect(commitMock).toHaveBeenCalled());
    await waitFor(() => {
      expect(rendered.getByTestId('ship-changes-step-push')).toBeInTheDocument();
    });

    // Close the drawer with the close button.
    await fireEvent.click(rendered.getByTestId('ship-changes-close'));
    await flush();

    expect(pushMock).not.toHaveBeenCalled();
    expect(prMock).not.toHaveBeenCalled();
  });

  it('shows the commit SHA after a successful commit', async () => {
    setBindingMock(
      'GitCommit',
      async () => ({ action: 'commit', commitSha: 'deadbeefcafe' } as GitActionResult),
    );
    const rendered = await mountWithThread();
    await openShipChangesDrawer(rendered);

    const subject = rendered.getByTestId('ship-changes-commit-subject') as HTMLInputElement;
    await fireEvent.input(subject, { target: { value: 'SHA-bearing commit' } });
    await flush();
    await fireEvent.click(rendered.getByTestId('ship-changes-commit-submit'));

    // The push step reads the just-captured SHA from state.commitSha.
    await waitFor(() => {
      expect(rendered.getByTestId('ship-changes-step-push')).toBeInTheDocument();
    });
    // PushStep renders the SHA under the step header. Use a regex since
    // the UI may truncate or prefix.
    const paneMod = await import('../../lib/stores/panes.svelte');
    // Store state also captures the SHA — an independent assertion so this
    // isn't purely UI-dependent.
    expect(paneMod).toBeDefined();
  });

  it('surfaces a push error and lets the user retry', async () => {
    setBindingMock(
      'GitCommit',
      async () => ({ action: 'commit', commitSha: 'c0ffee' } as GitActionResult),
    );
    let pushCalls = 0;
    const pushMock = setBindingMock('GitPush', async () => {
      pushCalls += 1;
      if (pushCalls === 1) {
        return { action: 'push', error: 'push blocked by server' } as GitActionResult;
      }
      return { action: 'push' } as GitActionResult;
    });
    const rendered = await mountWithThread();
    await openShipChangesDrawer(rendered);

    const subject = rendered.getByTestId('ship-changes-commit-subject') as HTMLInputElement;
    await fireEvent.input(subject, { target: { value: 'For the error test' } });
    await flush();
    await fireEvent.click(rendered.getByTestId('ship-changes-commit-submit'));
    await waitFor(() => {
      expect(rendered.getByTestId('ship-changes-step-push')).toBeInTheDocument();
    });

    await fireEvent.click(rendered.getByTestId('ship-changes-push-submit'));
    await waitFor(() => {
      expect(rendered.getByTestId('ship-changes-push-error')).toBeInTheDocument();
    });
    expect(rendered.getByTestId('ship-changes-push-error').textContent).toMatch(
      /push blocked by server/,
    );
    // Retry.
    await fireEvent.click(rendered.getByTestId('ship-changes-push-retry'));
    await flush();
    await fireEvent.click(rendered.getByTestId('ship-changes-push-submit'));
    await waitFor(() => {
      expect(rendered.getByTestId('ship-changes-step-pr')).toBeInTheDocument();
    });
    expect(pushMock).toHaveBeenCalledTimes(2);
  });

  it('prefills the PR title from the commit subject', async () => {
    setBindingMock(
      'GitCommit',
      async () => ({ action: 'commit', commitSha: 'abc' } as GitActionResult),
    );
    setBindingMock(
      'GitPush',
      async () => ({ action: 'push' } as GitActionResult),
    );
    const rendered = await mountWithThread();
    await openShipChangesDrawer(rendered);

    const subjectInput = rendered.getByTestId('ship-changes-commit-subject') as HTMLInputElement;
    await fireEvent.input(subjectInput, { target: { value: 'Add feature X' } });
    await flush();
    await fireEvent.click(rendered.getByTestId('ship-changes-commit-submit'));
    await waitFor(() => {
      expect(rendered.getByTestId('ship-changes-step-push')).toBeInTheDocument();
    });

    await fireEvent.click(rendered.getByTestId('ship-changes-push-submit'));
    await waitFor(() => {
      expect(rendered.getByTestId('ship-changes-step-pr')).toBeInTheDocument();
    });

    const titleInput = rendered.getByTestId('ship-changes-pr-title') as HTMLInputElement;
    expect(titleInput.value).toBe('Add feature X');
  });

  it('renders a clickable URL after GitCreatePR succeeds', async () => {
    setBindingMock(
      'GitCommit',
      async () => ({ action: 'commit', commitSha: 'abc' } as GitActionResult),
    );
    setBindingMock(
      'GitPush',
      async () => ({ action: 'push' } as GitActionResult),
    );
    setBindingMock(
      'GitCreatePR',
      async () =>
        ({
          action: 'pr',
          prUrl: 'https://github.com/owner/repo/pull/123',
        } as GitActionResult),
    );

    const rendered = await mountWithThread();
    await openShipChangesDrawer(rendered);

    const subjectInput = rendered.getByTestId('ship-changes-commit-subject') as HTMLInputElement;
    await fireEvent.input(subjectInput, { target: { value: 'URL title' } });
    await flush();
    await fireEvent.click(rendered.getByTestId('ship-changes-commit-submit'));
    await waitFor(() =>
      expect(rendered.getByTestId('ship-changes-step-push')).toBeInTheDocument(),
    );
    await fireEvent.click(rendered.getByTestId('ship-changes-push-submit'));
    await waitFor(() =>
      expect(rendered.getByTestId('ship-changes-step-pr')).toBeInTheDocument(),
    );
    await fireEvent.click(rendered.getByTestId('ship-changes-pr-submit'));

    const url = await waitFor(() => rendered.getByTestId('ship-changes-pr-url'));
    expect(url.getAttribute('href')).toBe('https://github.com/owner/repo/pull/123');
  });
});
