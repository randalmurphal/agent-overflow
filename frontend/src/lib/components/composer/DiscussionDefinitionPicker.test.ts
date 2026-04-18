import { beforeEach, describe, expect, it, vi } from 'vitest';
import { render, fireEvent, waitFor } from '@testing-library/svelte';
import DiscussionDefinitionPicker from './DiscussionDefinitionPicker.svelte';
import { setBindingMock } from '../../../test/mocks/bindings-app';
import type { DiscussionDefinition } from '../../types/discussion';

function def(
  overrides: Partial<DiscussionDefinition> = {},
): DiscussionDefinition {
  return {
    id: 'id-1',
    name: 'Architects',
    description: '',
    scope: 'global',
    projectId: undefined,
    participants: [],
    settings: { maxTurns: 4 },
    createdAt: 0,
    updatedAt: 0,
    ...overrides,
  };
}

beforeEach(() => {
  setBindingMock('ListDiscussions', async () => []);
});

describe('<DiscussionDefinitionPicker>', () => {
  it('renders the "None" option when no definitions exist', async () => {
    setBindingMock('ListDiscussions', async () => []);
    const { findByTestId } = render(DiscussionDefinitionPicker, {
      selectedName: null,
      projectPath: '/ws',
      onSelect: vi.fn(),
    });
    const select = (await findByTestId('discussion-definition-select')) as HTMLSelectElement;
    await waitFor(() => {
      expect(select.options[0].textContent).toMatch(/No discussions defined/i);
    });
  });

  it('shows project-scoped definitions in a Project optgroup', async () => {
    setBindingMock('ListDiscussions', async (scope: unknown) => {
      if (scope === 'project') return [def({ id: 'p1', name: 'TeamReview', scope: 'project', projectId: '/ws' })];
      return [];
    });
    const { findByTestId } = render(DiscussionDefinitionPicker, {
      selectedName: null,
      projectPath: '/ws',
      onSelect: vi.fn(),
    });
    const select = (await findByTestId('discussion-definition-select')) as HTMLSelectElement;
    await waitFor(() => {
      expect(select.querySelector('optgroup[label="Project"]')?.textContent).toContain('TeamReview');
    });
  });

  it('shows global definitions in a Global optgroup', async () => {
    setBindingMock('ListDiscussions', async (scope: unknown) => {
      if (scope === 'global') return [def({ id: 'g1', name: 'Architects', scope: 'global' })];
      return [];
    });
    const { findByTestId } = render(DiscussionDefinitionPicker, {
      selectedName: null,
      projectPath: '/ws',
      onSelect: vi.fn(),
    });
    const select = (await findByTestId('discussion-definition-select')) as HTMLSelectElement;
    await waitFor(() => {
      expect(select.querySelector('optgroup[label="Global"]')?.textContent).toContain('Architects');
    });
  });

  it('fires onSelect with the definition name when the user picks one', async () => {
    setBindingMock('ListDiscussions', async (scope: unknown) => {
      if (scope === 'global') return [def({ id: 'a', name: 'Architects', scope: 'global' })];
      return [];
    });
    const onSelect = vi.fn();
    const { findByTestId } = render(DiscussionDefinitionPicker, {
      selectedName: null,
      projectPath: '/ws',
      onSelect,
    });
    const select = (await findByTestId('discussion-definition-select')) as HTMLSelectElement;
    await waitFor(() => {
      expect(select.querySelector('optgroup[label="Global"]')).not.toBeNull();
    });
    await fireEvent.change(select, { target: { value: 'Architects' } });
    expect(onSelect).toHaveBeenCalledWith('Architects');
  });

  it('fires onSelect with null when the user picks the empty option', async () => {
    setBindingMock('ListDiscussions', async (scope: unknown) => {
      if (scope === 'global') return [def({ id: 'a', name: 'Architects', scope: 'global' })];
      return [];
    });
    const onSelect = vi.fn();
    const { findByTestId } = render(DiscussionDefinitionPicker, {
      selectedName: 'Architects',
      projectPath: '/ws',
      onSelect,
    });
    const select = (await findByTestId('discussion-definition-select')) as HTMLSelectElement;
    await waitFor(() => {
      expect(select.querySelector('optgroup[label="Global"]')).not.toBeNull();
    });
    await fireEvent.change(select, { target: { value: '' } });
    expect(onSelect).toHaveBeenCalledWith(null);
  });

  it('surfaces a load error', async () => {
    setBindingMock('ListDiscussions', async () => { throw new Error('db unavailable'); });
    const { findByTestId } = render(DiscussionDefinitionPicker, {
      selectedName: null,
      projectPath: '/ws',
      onSelect: vi.fn(),
    });
    const err = await findByTestId('discussion-definition-error');
    expect(err.textContent).toMatch(/db unavailable/);
  });

  it('is disabled when the `disabled` prop is true', async () => {
    setBindingMock('ListDiscussions', async () => [def()]);
    const { findByTestId } = render(DiscussionDefinitionPicker, {
      selectedName: null,
      projectPath: '/ws',
      onSelect: vi.fn(),
      disabled: true,
    });
    const select = (await findByTestId('discussion-definition-select')) as HTMLSelectElement;
    expect(select.disabled).toBe(true);
  });

  it('dedupes definitions appearing in both project and global lists', async () => {
    // A backend that returns the same id under both scopes (or a mock
    // that ignores the scope arg) would otherwise produce duplicate
    // options and trip Svelte's keyed-each invariant.
    setBindingMock('ListDiscussions', async () => [
      def({ id: 'shared', name: 'Architects', scope: 'global' }),
    ]);
    const { findByTestId } = render(DiscussionDefinitionPicker, {
      selectedName: null,
      projectPath: '/ws',
      onSelect: vi.fn(),
    });
    const select = (await findByTestId('discussion-definition-select')) as HTMLSelectElement;
    await waitFor(() => {
      const hits = Array.from(select.options).filter((o) => o.value === 'Architects');
      expect(hits.length).toBe(1);
    });
  });

  it('preserves unicode in definition names', async () => {
    setBindingMock('ListDiscussions', async (scope: unknown) => {
      if (scope === 'global') return [def({ id: 'u1', name: '建築家チーム', scope: 'global' })];
      return [];
    });
    const { findByTestId } = render(DiscussionDefinitionPicker, {
      selectedName: null,
      projectPath: '/ws',
      onSelect: vi.fn(),
    });
    const select = (await findByTestId('discussion-definition-select')) as HTMLSelectElement;
    await waitFor(() => {
      const opt = Array.from(select.options).find((o) => o.value === '建築家チーム');
      expect(opt).toBeDefined();
      expect(opt?.textContent).toBe('建築家チーム');
    });
  });

  it('filters project-scoped definitions to the current project path', async () => {
    setBindingMock('ListDiscussions', async (scope: unknown) => {
      if (scope === 'project') {
        return [
          def({ id: 'p1', name: 'MatchProject', scope: 'project', projectId: '/ws-a' }),
          def({ id: 'p2', name: 'OtherProject', scope: 'project', projectId: '/ws-b' }),
        ];
      }
      return [];
    });
    const { findByTestId } = render(DiscussionDefinitionPicker, {
      selectedName: null,
      projectPath: '/ws-a',
      onSelect: vi.fn(),
    });
    const select = (await findByTestId('discussion-definition-select')) as HTMLSelectElement;
    await waitFor(() => {
      const projectGroup = select.querySelector('optgroup[label="Project"]');
      expect(projectGroup?.textContent).toContain('MatchProject');
      expect(projectGroup?.textContent ?? '').not.toContain('OtherProject');
    });
  });

  it('does not fetch project-scoped discussions when projectPath is empty', async () => {
    const calls: string[] = [];
    setBindingMock('ListDiscussions', async (scope: unknown) => {
      calls.push(String(scope));
      return [];
    });
    render(DiscussionDefinitionPicker, {
      selectedName: null,
      projectPath: '',
      onSelect: vi.fn(),
    });
    await waitFor(() => expect(calls.length).toBeGreaterThan(0));
    // Only the global list is fetched when the project path is blank —
    // otherwise we'd hammer the backend for every keystroke in the
    // workspace field.
    expect(calls).not.toContain('project');
  });
});
