// Sorting by latest activity ranks each entry once per sort: a rank scans the
// entry's members and reads their live activity, which a comparator would
// repeat for every pair it compares.
import { beforeEach, expect, it, vi } from 'vitest';
import { render } from '@testing-library/svelte';
import { tick } from 'svelte';
import { readSidebarCatalogs } from '../../../../test/helpers/sidebarCatalogs';
import { setBindingMock } from '../../../../test/mocks/bindings-app';
import { touchProjectActivity } from '../../../stores/projects.svelte';
import { setProjectSortMode } from '../../../stores/sidebar.svelte';
import type { ProjectWithCounts } from '../../../types/models';

const ranked = vi.hoisted(() => ({ calls: 0 }));

// A rank reads each member's live activity once; every entry here has one
// member, so the reads count the ranks.
vi.mock('../../../stores/projects.svelte', async (importOriginal) => {
  const real = await importOriginal<typeof import('../../../stores/projects.svelte')>();
  return {
    ...real,
    getProjectLiveActivityAt: (row: Parameters<typeof real.getProjectLiveActivityAt>[0]) => {
      ranked.calls += 1;
      return real.getProjectLiveActivityAt(row);
    },
  };
});

import ProjectsSection from '../ProjectsSection.svelte';

const COUNT = 8;

function row(index: number): ProjectWithCounts {
  return {
    project: { id: `p${index}`, name: `p${index}`, path: `/tmp/p${index}`, sortPosition: 0, createdAt: 0, updatedAt: 0, archived: false },
    threadCount: 0,
    lastActive: index,
  };
}

beforeEach(() => {
  setProjectSortMode('lastActivity');
});

it('ranks each entry once when a beat re-sorts the list', async () => {
  setBindingMock('ListProjects', async () => Array.from({ length: COUNT }, (_, index) => row(index)));
  const { container } = render(ProjectsSection, { props: { pane: null } });
  await readSidebarCatalogs();
  for (let i = 0; i < 4; i++) await tick();
  expect(container.querySelectorAll('[data-testid="project-item"]')).toHaveLength(COUNT);

  ranked.calls = 0;
  touchProjectActivity('p0', 1000);
  for (let i = 0; i < 4; i++) await tick();

  expect(container.querySelector('[data-testid="project-item"]')?.getAttribute('data-project-id')).toBe('p0');
  expect(ranked.calls).toBe(COUNT);
});
