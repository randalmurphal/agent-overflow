// A run's life: one live run per row, and nothing outside it can act.
import { describe, expect, it } from 'vitest';
import { createProjectActions, type ProjectActionKind } from './projectActions.svelte';
import type { ProjectTarget } from './sidebarEntries';
import { HOME_BACKEND } from '../../transport/backendKey';
import type { ProjectDeletionPreview } from '../../types/workflow';

function target(id: string): ProjectTarget {
  return Object.freeze({
    id,
    name: id,
    path: `/repo/${id}`,
    backend: HOME_BACKEND,
    label: id,
    threadCount: 1,
  });
}

function preview(id: string): ProjectDeletionPreview {
  return { projectId: id, runCount: 1, liveRunIds: [], worktrees: [], hasWork: true };
}

/**
 * Every way a run can still try to act, after it should not be able to.
 * What it collected while it was live stays as it is: the point is that
 * none of these calls changes anything, `beginSubmit` above all.
 */
function refusesEverything(run: ReturnType<ReturnType<typeof createProjectActions>['start']>): void {
  expect(run.retired).toBe(true);
  expect(run.beginSubmit()).toBe(false);
  const before = { confirming: run.confirming, preview: run.preview, name: run.name };
  run.confirm();
  run.offer(preview('late'));
  run.setName('typed after the fact');
  expect({ confirming: run.confirming, preview: run.preview, name: run.name }).toEqual(before);
}

describe('createProjectActions', () => {
  // The hole this closes: ending a run only took it off screen. A
  // confirmation callback captured before the user cancelled could still
  // submit it, on a row that had moved on to something else.
  for (const kind of ['archive', 'delete', 'rename'] as ProjectActionKind[]) {
    it(`refuses a ${kind} run that was ended before the next one started`, () => {
      const actions = createProjectActions();
      const ended = actions.start(kind, target('a'));
      ended.end();
      expect(actions.current).toBeNull();

      const next = actions.start(kind === 'rename' ? 'delete' : 'rename', target('b'));
      expect(actions.current).toBe(next);
      refusesEverything(ended);
      // And the run that replaced it is untouched by any of that.
      expect(next.retired).toBe(false);
      expect(actions.current).toBe(next);
    });
  }

  it('refuses a run a newer one replaced', () => {
    const actions = createProjectActions();
    const first = actions.start('delete', target('a'));
    first.confirm();
    const second = actions.start('archive', target('b'));
    refusesEverything(first);
    expect(second.beginSubmit()).toBe(true);
  });

  it('refuses a second submit of the run that is current', () => {
    const actions = createProjectActions();
    const run = actions.start('archive', target('a'));
    run.confirm();
    expect(run.beginSubmit()).toBe(true);
    // Its call is out: it is the current run, and it cannot act again.
    expect(actions.current).toBe(run);
    expect(run.submitting).toBe(true);
    refusesEverything(run);
  });

  it('keeps nothing live that is not current', () => {
    const actions = createProjectActions();
    const run = actions.start('rename', target('a'));
    run.setName('renamed');
    expect(run.name).toBe('renamed');
    expect(run.retired).toBe(false);
    run.end();
    expect(actions.current).toBeNull();
    refusesEverything(run);
  });
});
