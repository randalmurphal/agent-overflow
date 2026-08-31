// Duplicate project names are legal (paths are the unique key). The row's
// label reads the store's disambiguation map: a unique name renders bare
// (with the classic truncate span), a duplicate gains a dim parent-dir
// prefix that must never ellipsize.

import { afterEach, describe, expect, it } from 'vitest';
import { cleanup, render } from '@testing-library/svelte';
import ProjectItem from './ProjectItem.svelte';
import { addProjectLocal, resetProjectsForTest } from '../../stores/projects.svelte';
import { pairViewOnly, resetToLocalPage } from '../../../test/helpers/scopes';
import type { Project, ProjectWithCounts } from '../../types/models';

function makeProject(id: string, name: string, path: string): Project {
  return {
    id,
    name,
    path,
    slug: id,
    color: '',
    sortPosition: 0,
    createdAt: 1,
    updatedAt: 1,
    archived: false,
  } as Project;
}

function withCounts(p: Project): ProjectWithCounts {
  return { project: p, threadCount: 0, lastActive: 0 };
}

function renderItem(p: Project) {
  return render(ProjectItem, {
    props: { project: withCounts(p), threads: [], pane: null } as never,
  });
}

afterEach(() => {
  cleanup();
  resetProjectsForTest();
  resetToLocalPage();
});

// The row's two create controls stay in place for a session that cannot use
// them — a project whose row lost half its affordances reads as a broken
// sidebar rather than a read-only one — and go inert instead.
describe('ProjectItem create controls', () => {
  it('offers both on the local page', () => {
    const p = makeProject('a', 'web', '/work/web');
    addProjectLocal(p);
    const { getByTestId } = renderItem(p);
    expect((getByTestId('project-item-new-thread') as HTMLButtonElement).disabled).toBe(false);
    expect((getByTestId('project-item-new-terminal') as HTMLButtonElement).disabled).toBe(false);
  });

  it('renders them inert for a view-only session', async () => {
    const p = makeProject('a', 'web', '/work/web');
    addProjectLocal(p);
    await pairViewOnly();
    const { getByTestId } = renderItem(p);
    const newThread = getByTestId('project-item-new-thread') as HTMLButtonElement;
    expect(newThread.disabled).toBe(true);
    expect(newThread.title).toBe('Local only');
    expect((getByTestId('project-item-new-terminal') as HTMLButtonElement).disabled).toBe(true);
  });
});

describe('ProjectItem label', () => {
  it('renders a unique name bare', () => {
    const p = makeProject('a', 'web', '/work/web');
    addProjectLocal(p);
    const { getByTestId } = renderItem(p);
    const label = getByTestId('project-item-label');
    expect(label).toHaveTextContent('web');
    expect(label.textContent).not.toContain('/');
  });

  it('renders a parent-dir prefix when another project shares the name', () => {
    const a = makeProject('a', 'web', '/work/clients/web');
    const b = makeProject('b', 'web', '/work/personal/web');
    addProjectLocal(a);
    addProjectLocal(b);
    const { getByTestId } = renderItem(a);
    const label = getByTestId('project-item-label');
    expect(label.textContent?.replace(/\s+/g, '')).toBe('clients/web');
    // No ellipsis on the conflict layout — the prefix exists to be read.
    expect(label.className).not.toContain('truncate');
    expect(label.querySelector('.truncate')).toBeNull();
  });

  it('drops the prefix again once the conflicting project is gone', async () => {
    const a = makeProject('a', 'web', '/work/clients/web');
    const b = makeProject('b', 'web', '/work/personal/web');
    addProjectLocal(a);
    addProjectLocal(b);
    const view = renderItem(a);
    resetProjectsForTest();
    addProjectLocal(a);
    await Promise.resolve();
    const label = view.getByTestId('project-item-label');
    expect(label.textContent?.replace(/\s+/g, '')).toBe('web');
  });
});
