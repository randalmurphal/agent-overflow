import { afterEach, beforeEach, describe, expect, it } from 'vitest';

import { checkoutRefusal } from './computerProjects';
import { refreshProjects, resetProjectsForTest } from './projects.svelte';
import { resetBindingMocks, setBindingMock } from '../../test/mocks/bindings-app';
import { takePinnedBackend } from '../transport/backends';
import { __resetEntityIndexForTest } from '../transport/entityIndex';
import type { Project } from '../types/models';

function makeProject(overrides: Partial<Project>): Project {
  return { id: 'p', path: '/repo', name: 'app', sortPosition: 0, createdAt: 0, updatedAt: 0, archived: false, ...overrides };
}

async function seed(project: Project): Promise<void> {
  setBindingMock('ListProjects', async () => [{ project, threadCount: 0 }]);
  await refreshProjects();
}

function inspectAs(answer: () => Promise<unknown>) {
  return setBindingMock('InspectProjectFolder', async (path: string) => {
    expect(takePinnedBackend()).toBe('laptop');
    expect(path).toBe('/home/me/app');
    return answer();
  });
}

describe('checkoutRefusal', () => {
  beforeEach(() => {
    resetBindingMocks();
    resetProjectsForTest();
    __resetEntityIndexForTest();
  });
  afterEach(() => resetProjectsForTest());

  it('accepts a checkout of the same repository under another spelling of its remote', async () => {
    await seed(makeProject({ remoteURL: 'git@github.com:me/app.git', rootCommit: 'abc' }));
    const inspect = inspectAs(async () => ({ repository: true, remoteURL: 'https://github.com/me/app', rootCommit: 'abc' }));
    expect(await checkoutRefusal('p', 'laptop', '/home/me/app')).toBeNull();
    expect(inspect).toHaveBeenCalledTimes(1);
  });

  it('accepts a remoteless checkout of the same history', async () => {
    await seed(makeProject({ remoteURL: 'git@github.com:me/app.git', rootCommit: 'abc' }));
    inspectAs(async () => ({ repository: true, rootCommit: 'abc' }));
    expect(await checkoutRefusal('p', 'laptop', '/home/me/app')).toBeNull();
  });

  it('names both repositories when the folder is a checkout of another one', async () => {
    await seed(makeProject({ remoteURL: 'git@github.com:me/app.git', rootCommit: 'abc' }));
    inspectAs(async () => ({ repository: true, remoteURL: 'git@github.com:me/other.git', rootCommit: 'def' }));
    expect(await checkoutRefusal('p', 'laptop', '/home/me/app'))
      .toBe('That folder is a checkout of github.com/me/other, not github.com/me/app.');
  });

  it('refuses a remoteless checkout of another history', async () => {
    await seed(makeProject({ remoteURL: 'git@github.com:me/app.git', rootCommit: 'abc' }));
    inspectAs(async () => ({ repository: true, rootCommit: 'def' }));
    expect(await checkoutRefusal('p', 'laptop', '/home/me/app')).toBe("That folder isn't a checkout of github.com/me/app.");
  });

  it('refuses a folder that is not a repository', async () => {
    await seed(makeProject({ rootCommit: 'abc' }));
    inspectAs(async () => ({ repository: false }));
    expect(await checkoutRefusal('p', 'laptop', '/home/me/app'))
      .toBe("That folder isn't a git repository, so it can't be app.");
  });

  it('refuses with git\'s reason when the folder cannot be read', async () => {
    await seed(makeProject({ remoteURL: 'git@github.com:me/app.git' }));
    inspectAs(async () => { throw new Error('detected dubious ownership'); });
    expect(await checkoutRefusal('p', 'laptop', '/home/me/app'))
      .toBe("Git couldn't read that folder: detected dubious ownership");
  });

  it('accepts any folder for a project with no repository identity, without reading it', async () => {
    await seed(makeProject({}));
    const inspect = setBindingMock('InspectProjectFolder', async () => ({ repository: false }));
    expect(await checkoutRefusal('p', 'laptop', '/anything')).toBeNull();
    expect(inspect).not.toHaveBeenCalled();
  });
});
