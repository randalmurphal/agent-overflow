// The discussion definitions screen reads its list with a plain RPC and had no
// way to hear about anyone else's write: create, rename, edit and delete all
// persisted and answered their own caller, so a definition written on one
// device never appeared here until the screen was reopened.
//
// `discussion:definitions-changed` moves a revision counter and this screen
// re-runs its own load off it. There is no shared definitions store on purpose
// (two surfaces read two different slices of the list), so the counter is the
// whole mechanism and this is where it is proved end to end.

import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { cleanup, render, waitFor } from '@testing-library/svelte';
import { resetBindingMocks, setBindingMock } from '../../../test/mocks/bindings-app';
import {
  applyDiscussionDefinitionsChanged,
  resetDiscussionDefinitionsForTest,
} from '../../stores/discussionDefinitions.svelte';
import DiscussionsSettings from './DiscussionsSettings.svelte';
import type { DiscussionDefinition, DiscussionScope } from '../../types/discussion';

function definition(name: string, scope: DiscussionScope = 'global'): DiscussionDefinition {
  return {
    name,
    scope,
    description: '',
    participants: [{ role: 'architect', description: '', system: '', provider: 'claude', model: '' }],
    settings: { maxTurns: 4 },
  } as DiscussionDefinition;
}

let globalRows: DiscussionDefinition[] = [];

beforeEach(() => {
  resetBindingMocks();
  resetDiscussionDefinitionsForTest();
  globalRows = [definition('architects')];
  setBindingMock('ListDiscussions', async (scope: string) =>
    scope === 'global' ? globalRows : [],
  );
});

afterEach(() => {
  cleanup();
  resetDiscussionDefinitionsForTest();
});

describe('DiscussionsSettings', () => {
  it('re-reads the list when another client writes a definition', async () => {
    const { getByText, queryByText } = render(DiscussionsSettings);
    await waitFor(() => expect(getByText('architects')).toBeTruthy());
    expect(queryByText('reviewers')).toBeNull();

    globalRows = [definition('architects'), definition('reviewers')];
    applyDiscussionDefinitionsChanged();

    await waitFor(() => expect(getByText('reviewers')).toBeTruthy());
  });

  it('drops a definition another client deleted', async () => {
    const { getByText, queryByText } = render(DiscussionsSettings);
    await waitFor(() => expect(getByText('architects')).toBeTruthy());

    globalRows = [];
    applyDiscussionDefinitionsChanged();

    await waitFor(() => expect(queryByText('architects')).toBeNull());
  });

  // A local save calls the load with a selection to restore and the wire
  // signal calls it without one, so the two can be in flight together: an
  // older completion must drop rather than install the list it fetched first.
  it('lets the newest load win when two are in flight', async () => {
    let releaseFirst!: () => void;
    const firstAnswer = new Promise<void>((resolve) => {
      releaseFirst = resolve;
    });
    let call = 0;
    setBindingMock('ListDiscussions', async (scope: string) => {
      call += 1;
      if (call <= 2) {
        await firstAnswer;
        return scope === 'global' ? [definition('stale')] : [];
      }
      return scope === 'global' ? [definition('fresh')] : [];
    });

    const { getByText, queryByText } = render(DiscussionsSettings);
    applyDiscussionDefinitionsChanged();
    await waitFor(() => expect(call).toBeGreaterThanOrEqual(4));
    releaseFirst();

    await waitFor(() => expect(getByText('fresh')).toBeTruthy());
    expect(queryByText('stale')).toBeNull();
  });
});
