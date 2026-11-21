// A run's life: one live run per row, and nothing outside it can act.
import { describe, expect, it } from 'vitest';
import {
  createProjectActions,
  type ProjectActionKind,
  type ProjectRunOf,
} from './projectActions.svelte';
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
  // Ending a run must take it out of the row's life, not only off screen:
  // a confirmation callback captured before the user cancelled holds the
  // run, and the row it would submit on has moved to something else.
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

  // Raising a failed call's dialog again belongs to the owner: a plain
  // `start` would put a project the user has moved on from back on the row.
  describe('reraiseDelete', () => {
    /** A preview delete taken for its call: the run outlives it. */
    function submitted(actions: ReturnType<typeof createProjectActions>, id: string) {
      const run = actions.start('delete', target(id)) as ProjectRunOf<'delete'>;
      run.offer(preview(id));
      expect(run.beginSubmit()).toBe(true);
      return run;
    }

    it('raises the dialog again while the failed run still owns the row', () => {
      const actions = createProjectActions();
      const run = submitted(actions, 'a');

      const again = actions.reraiseDelete(run);

      expect(again).not.toBeNull();
      expect(actions.current).toBe(again);
      // The same project and the same answer, as a run that can be sent.
      expect(again?.target).toBe(run.target);
      expect(again?.preview).toEqual(preview('a'));
      expect(again?.retired).toBe(false);
      expect(again?.beginSubmit()).toBe(true);
    });

    it('brings nothing back after the user closed the dialog', () => {
      const actions = createProjectActions();
      const run = submitted(actions, 'a');
      run.end();

      expect(actions.reraiseDelete(run)).toBeNull();
      expect(actions.current).toBeNull();
    });

    it('cannot be handed a run of another kind', () => {
      const actions = createProjectActions();
      const archive = actions.start('archive', target('a'));
      archive.end();

      // @ts-expect-error an archive run is not a delete's. Nothing a failed
      // archive or rename collects outlives its call, so there is no dialog
      // of theirs to raise again, and no call site may pass one here.
      expect(actions.reraiseDelete(archive)).toBeNull();
    });

    it('leaves a newer action alone', () => {
      const actions = createProjectActions();
      const run = submitted(actions, 'a');
      const rename = actions.start('rename', target('b'));

      expect(actions.reraiseDelete(run)).toBeNull();
      expect(actions.current).toBe(rename);
      expect(rename.retired).toBe(false);
    });
  });
});
