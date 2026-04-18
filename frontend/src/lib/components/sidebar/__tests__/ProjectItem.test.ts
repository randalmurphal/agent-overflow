import { beforeEach, describe, expect, it, vi } from 'vitest';
import { render, fireEvent } from '@testing-library/svelte';
import { tick } from 'svelte';
import ProjectItem from '../ProjectItem.svelte';
import { createThreadPane } from '../../../stores/thread.svelte';
import {
  expandProject,
  isProjectExpanded,
  resetSidebarForTest,
} from '../../../stores/sidebar.svelte';
import type { Project, ProjectWithCounts } from '../../../types/models';

function wrap(id: string, overrides: Partial<Project> = {}): ProjectWithCounts {
  return {
    project: {
      id,
      path: `/tmp/${id}`,
      name: overrides.name ?? id,
      sortPosition: 0,
      createdAt: 0,
      updatedAt: 0,
      archived: false,
      ...overrides,
    },
    threadCount: 0,
    lastActive: 0,
  };
}

describe('<ProjectItem>', () => {
  beforeEach(() => {
    resetSidebarForTest();
  });

  it('renders collapsed by default and exposes an aria-expanded chevron', () => {
    const pane = createThreadPane();
    const { getByTestId } = render(ProjectItem, {
      props: {
        project: wrap('p1', { name: 'Alpha' }),
        threads: [],
        pane,
      },
    });
    const chevron = getByTestId('project-item-chevron') as HTMLButtonElement;
    expect(chevron.getAttribute('aria-expanded')).toBe('false');
    expect(isProjectExpanded('p1')).toBe(false);
  });

  it('clicking the chevron expands the project', async () => {
    const pane = createThreadPane();
    const { getByTestId } = render(ProjectItem, {
      props: {
        project: wrap('p1'),
        threads: [],
        pane,
      },
    });
    await fireEvent.click(getByTestId('project-item-chevron'));
    await tick();
    expect(isProjectExpanded('p1')).toBe(true);
  });

  it('pencil button is hidden by default but invokes onNewThread when clicked', async () => {
    const onNewThread = vi.fn();
    const pane = createThreadPane();
    const { getByTestId } = render(ProjectItem, {
      props: {
        project: wrap('p1'),
        threads: [],
        pane,
        onNewThread,
      },
    });
    const pencil = getByTestId('project-item-new-thread') as HTMLButtonElement;
    // Visual hover state is CSS-only; the test asserts the class lives,
    // not that it's currently applied (happy-dom can't simulate :hover).
    expect(pencil.className).toContain('group-hover:opacity-100');
    await fireEvent.click(pencil);
    expect(onNewThread).toHaveBeenCalledWith('p1');
  });

  it('renders nested threads when expanded', async () => {
    const pane = createThreadPane();
    expandProject('p1');
    const { getByTestId } = render(ProjectItem, {
      props: {
        project: wrap('p1'),
        threads: [],
        pane,
      },
    });
    await tick();
    expect(getByTestId('project-thread-list-empty')).toBeInTheDocument();
  });

  it('clicking the pencil does not bubble to the row toggle', async () => {
    const onNewThread = vi.fn();
    const pane = createThreadPane();
    const { getByTestId } = render(ProjectItem, {
      props: {
        project: wrap('p1'),
        threads: [],
        pane,
        onNewThread,
      },
    });
    await fireEvent.click(getByTestId('project-item-new-thread'));
    // The click should not have toggled expansion — only onNewThread fires.
    expect(isProjectExpanded('p1')).toBe(false);
    expect(onNewThread).toHaveBeenCalledTimes(1);
  });
});
