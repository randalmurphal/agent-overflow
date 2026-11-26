// A thread's linked ticket's mark draws its project's status groups: the
// setup store loads only once a thread has a link, one hold and one read
// per project with a linked thread, released when its last linked thread
// goes, and a thread header holds its thread's project while it shows.

import { afterEach, describe, expect, it, vi } from 'vitest';
import { flushSync } from 'svelte';
import type { SetupView } from '../../../bindings/agent-overflow/internal/trackerapp/models';
import type { ThreadTicketLink } from '../../../bindings/agent-overflow/internal/store/models';
import { makeThread } from '../../test/helpers/chat';
import { setBindingMock } from '../../test/mocks/bindings-app';
import { HOME_BACKEND } from '../transport/backendKey';
import {
  holdThreadLinkProject,
  keepLinkedThreadSetups,
  linkMarkGroup,
  linkedThreadSetupsLoadStartedForTest,
  releaseLinkedThreadSetups,
} from './threadLinkMarks.svelte';
import { replaceAllThreads } from './threads.svelte';
import { trackerSetupUpdated } from './trackerBoardEvents';
import { __setBackendStatusForTest } from './transportStatus.svelte';

const SITE = 'https://example.atlassian.net';

function link(statusId = '3', statusGroup = 'in_progress'): ThreadTicketLink {
  return {
    kind: 'jira', site: SITE, issueId: '10001', key: 'WEB-1', title: 'First story',
    statusId, statusName: 'In Progress', statusGroup, origin: 'manual', linkedAt: 1, state: 'current', connectionId: 'conn-1',
  };
}

function view(groups: Record<string, string> = {}): SetupView {
  return { revision: 'r1', setup: { status_groups: [{ tracker: 'jira', site: SITE, statuses: groups }], ticket_pins: [], start_work: [] } } as SetupView;
}

let stop: (() => void) | null = null;
function keep(): void {
  stop = $effect.root(() => {
    $effect(() => keepLinkedThreadSetups());
  });
  flushSync();
}

afterEach(() => {
  stop?.();
  stop = null;
  releaseLinkedThreadSetups();
});

describe("a linked ticket's mark", () => {
  it('loads no setup code and reads no setup in a window with no linked thread', async () => {
    const reads = setBindingMock('ProjectTrackerSetup', async () => view());
    replaceAllThreads([makeThread({ id: 't1', projectId: 'p1' }), makeThread({ id: 't2', projectId: 'p2' })]);
    keep();
    await Promise.resolve();
    expect(linkedThreadSetupsLoadStartedForTest(), 'the setup store was asked for').toBe(false);
    expect(reads).not.toHaveBeenCalled();
  });

  it('reads once per project with a linked thread, and draws the group the project puts the status in', async () => {
    const reads = setBindingMock('ProjectTrackerSetup', async (project: string) => view(project === 'p1' ? { 3: 'done' } : { 3: 'left_out' }));
    const t1 = makeThread({ id: 't1', projectId: 'p1', ticketLink: link() });
    const t3 = makeThread({ id: 't3', projectId: 'p2', ticketLink: link() });
    replaceAllThreads([t1, makeThread({ id: 't2', projectId: 'p1', ticketLink: link() }), t3, makeThread({ id: 't4', projectId: 'p3' })]);
    expect(linkMarkGroup(t1), 'the tracker\'s group until the setup is read').toBe('in_progress');
    keep();
    await vi.waitFor(() => expect(reads).toHaveBeenCalledTimes(2));
    expect(reads.mock.calls.map((c) => c[0]).sort(), 'none for p3, which has no linked thread').toEqual(['p1', 'p2']);
    await vi.waitFor(() => expect(linkMarkGroup(t1)).toBe('done'));
    expect(linkMarkGroup(t3), 'a status left out of counts keeps the tracker\'s mark').toBe('in_progress');
    expect(linkMarkGroup(makeThread({ id: 't5', projectId: 'p1' }))).toBeUndefined();
  });

  it('releases a project\'s hold when its last linked thread goes, and holds again when one is linked', async () => {
    const reads = setBindingMock('ProjectTrackerSetup', async () => view({ 3: 'done' }));
    const linked = makeThread({ id: 't1', projectId: 'p1', ticketLink: link() });
    replaceAllThreads([linked]);
    keep();
    await vi.waitFor(() => expect(linkMarkGroup(linked)).toBe('done'));
    replaceAllThreads([makeThread({ id: 't1', projectId: 'p1', ticketLink: null })]);
    flushSync();
    expect(linkMarkGroup(linked), 'released: the mark draws the tracker\'s group').toBe('in_progress');
    trackerSetupUpdated(HOME_BACKEND);
    await Promise.resolve();
    expect(reads, 'nothing reads a released project on the event').toHaveBeenCalledTimes(1);
    replaceAllThreads([linked]);
    flushSync();
    await vi.waitFor(() => expect(reads).toHaveBeenCalledTimes(2));
  });

  it('holds the project of a thread a header shows that the list does not', async () => {
    const reads = setBindingMock('ProjectTrackerSetup', async () => view({ 3: 'done' }));
    replaceAllThreads([]);
    keep();
    const shown = makeThread({ id: 't9', projectId: 'p9', ticketLink: link() });
    // As a header holds it: from an effect, beside a second header.
    let showing = $state(true);
    const header = $effect.root(() => {
      $effect(() => holdThreadLinkProject(showing ? shown.projectId : null));
    });
    const second = holdThreadLinkProject(shown.projectId);
    flushSync();
    await vi.waitFor(() => expect(linkMarkGroup(shown)).toBe('done'));
    expect(reads).toHaveBeenCalledTimes(1);
    showing = false;
    flushSync();
    expect(linkMarkGroup(shown), 'the second header still holds it').toBe('done');
    second();
    flushSync();
    expect(linkMarkGroup(shown)).toBe('in_progress');
    header();
  });

  it('reads each held project once when its computer connects again: three projects, three reads', async () => {
    const reads = setBindingMock('ProjectTrackerSetup', async () => view());
    replaceAllThreads(['p1', 'p2', 'p3'].map((p, i) => makeThread({ id: `t${i}`, projectId: p, ticketLink: link() })));
    keep();
    await vi.waitFor(() => expect(reads).toHaveBeenCalledTimes(3));
    __setBackendStatusForTest(HOME_BACKEND, { status: 'disconnected', nextAttemptAt: null });
    __setBackendStatusForTest(HOME_BACKEND, { status: 'connected', nextAttemptAt: null });
    await vi.waitFor(() => expect(reads).toHaveBeenCalledTimes(6));
  });
});
