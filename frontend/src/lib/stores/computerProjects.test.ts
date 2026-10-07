import { afterEach, beforeEach, describe, expect, it } from 'vitest';

import { addComputerCheckout, checkoutRefusal } from './computerProjects';
import { getProject, refreshProjects, resetProjectsForTest } from './projects.svelte';
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

  it('accepts a checkout of the same verified repository', async () => {
    await seed(makeProject({ repositoryID: 'github:github.com:1' }));
    const inspect = inspectAs(async () => ({ repository: true, repositoryID: 'github:github.com:1' }));
    expect(await checkoutRefusal('p', 'laptop', '/home/me/app')).toBeNull();
    expect(inspect).toHaveBeenCalledTimes(1);
  });

  it('accepts an already verified checkout while the forge is offline', async () => {
    await seed(makeProject({ repositoryID: 'github:github.com:1' }));
    inspectAs(async () => ({ repository: true, repositoryID: 'github:github.com:1', identityError: 'Forge unavailable' }));
    expect(await checkoutRefusal('p', 'laptop', '/home/me/app')).toBeNull();
  });

  it('refuses an unverified checkout even when an older backend supplies matching URL and history', async () => {
    await seed(makeProject({ repositoryID: 'github:github.com:1' }));
    inspectAs(async () => ({ repository: true, remoteURL: 'https://github.com/me/app', rootCommit: 'abc', identityError: 'GitHub unavailable' }));
    expect(await checkoutRefusal('p', 'laptop', '/home/me/app')).toBe('Could not verify that checkout: GitHub unavailable');
  });

  it('refuses a checkout with a different verified ID', async () => {
    await seed(makeProject({ repositoryID: 'github:github.com:1' }));
    inspectAs(async () => ({ repository: true, repositoryID: 'github:github.com:2' }));
    expect(await checkoutRefusal('p', 'laptop', '/home/me/app'))
      .toBe("That folder isn't a verified checkout of app.");
  });

  it('refuses a folder that is not a repository', async () => {
    await seed(makeProject({ repositoryID: 'github:github.com:1' }));
    inspectAs(async () => ({ repository: false }));
    expect(await checkoutRefusal('p', 'laptop', '/home/me/app'))
      .toBe("That folder isn't a git repository, so it can't be app.");
  });

  it('refuses with git\'s reason when the folder cannot be read', async () => {
    await seed(makeProject({ repositoryID: 'github:github.com:1' }));
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
  it('refreshes the source on its computer and validates registration on the destination', async () => {
    const source = makeProject({ repositoryID: 'github:github.com:42' });
    const destination = makeProject({ id: 'destination', path: '/home/me/app', repositoryID: source.repositoryID });
    await seed(source);
    const calls: string[] = [];
    setBindingMock('RefreshProjectIdentity', async (id: string) => {
      expect(takePinnedBackend()).toBe(''); expect(id).toBe('p'); calls.push('refresh'); return source;
    });
    inspectAs(async () => { calls.push('inspect'); return { repository: true, ...destination }; });
    setBindingMock('CreateProjectCheckout', async (path: string, expected: unknown) => {
      expect(takePinnedBackend()).toBe('laptop'); expect(path).toBe(destination.path);
      expect(expected).toMatchObject({ repositoryID: source.repositoryID });
      calls.push('create'); return destination;
    });
    expect((await addComputerCheckout('p', 'laptop', destination.path)).id).toBe(destination.id);
    expect(calls).toEqual(['refresh', 'inspect', 'create']);
    expect(getProject(destination.id)?.project).toEqual(destination);
  });

  it('surfaces a registration refusal after preflight without adding a project', async () => {
    const source = makeProject({ repositoryID: 'github:github.com:42' });
    await seed(source);
    setBindingMock('RefreshProjectIdentity', async () => { takePinnedBackend(); return source; });
    inspectAs(async () => ({ repository: true, repositoryID: source.repositoryID }));
    setBindingMock('CreateProjectCheckout', async () => { takePinnedBackend(); throw new Error('checkout changed'); });
    await expect(addComputerCheckout('p', 'laptop', '/home/me/app')).rejects.toThrow('checkout changed');
    expect(getProject('destination')).toBeUndefined();
  });

});
