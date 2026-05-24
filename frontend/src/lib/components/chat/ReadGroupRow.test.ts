import { describe, expect, it } from 'vitest';
import { render } from '@testing-library/svelte';
import ReadGroupRow from './ReadGroupRow.svelte';
import type { Item } from '../../types/models';
import type { ReadGroupNode } from '../../utils/subagentGrouping';
import type { ThreadPane } from '../../stores/thread.svelte';

function mkReadItem(id: string, summary: string, overrides: Partial<Item> = {}): Item {
  return {
    id,
    threadId: 'thread-1',
    turnIndex: 0,
    itemIndex: 0,
    kind: 'tool_call',
    role: 'assistant',
    status: 'completed',
    toolName: 'Read',
    summary,
    createdAt: 0,
    updatedAt: 0,
    ...overrides,
  };
}

function mkGroup(members: Item[]): ReadGroupNode {
  return {
    kind: 'read_group',
    groupKey: `reads:${members[0].id}`,
    threadId: 'thread-1',
    members,
  };
}

function paneWithWorkspace(path: string): ThreadPane {
  return { thread: { workspacePath: path } } as unknown as ThreadPane;
}

describe('<ReadGroupRow>', () => {
  it('renders one EditorLink per member with basename labels and workspace-relative click targets', () => {
    const group = mkGroup([
      mkReadItem('r1', 'Read: /home/me/repo/src/foo.ts'),
      mkReadItem('r2', 'Read: /home/me/repo/src/bar.ts'),
      mkReadItem('r3', 'Read: /home/me/repo/test/baz_test.ts'),
    ]);
    const { getAllByTestId, getByTestId } = render(ReadGroupRow, {
      props: { pane: paneWithWorkspace('/home/me/repo'), group },
    });

    // Each member surfaces as its own inline EditorLink so the user can
    // click any single file without expanding a body. The list slot
    // wraps via CSS (flex-wrap) — the rendered DOM is just a flat list.
    const links = getAllByTestId('editor-link');
    expect(links).toHaveLength(3);
    // data-path keeps the workspace-relative form so EditorLink's
    // workspacePath prop can join it back to absolute for OpenInEditor.
    expect(links.map((el) => el.getAttribute('data-path'))).toEqual([
      'src/foo.ts',
      'src/bar.ts',
      'test/baz_test.ts',
    ]);
    // The displayed label collapses to the basename so a wide directory
    // tree doesn't blow out the inline row.
    expect(links.map((el) => el.textContent?.trim())).toEqual([
      'foo.ts',
      'bar.ts',
      'baz_test.ts',
    ]);
    expect(links[0].className).toContain('break-all');
    expect(links[0].getAttribute('title')).toBe('Open foo.ts in editor');
    expect(getByTestId('read-group-row-label').textContent).toBe('reads');
  });

  it('leaves paths outside the workspace absolute so EditorLink can resolve them', () => {
    // Reads of system files (rare but real — agent surveying a
    // dependency under /usr or a sibling repo) must stay openable.
    // EditorLink consumes `data-path` directly when it's absolute, and
    // the visible label stays absolute so outside-repo reads are not
    // confused with same-named project files.
    const group = mkGroup([
      mkReadItem('r1', 'Read: /usr/local/share/foo.go'),
      mkReadItem('r2', 'Read: /home/me/repo/src/bar.ts'),
    ]);
    const { getAllByTestId } = render(ReadGroupRow, {
      props: { pane: paneWithWorkspace('/home/me/repo'), group },
    });

    const links = getAllByTestId('editor-link');
    expect(links.map((el) => el.getAttribute('data-path'))).toEqual([
      '/usr/local/share/foo.go',
      'src/bar.ts',
    ]);
    expect(links.map((el) => el.textContent?.trim())).toEqual([
      '/usr/local/share/foo.go',
      'bar.ts',
    ]);
  });

  it('shows repo-relative paths when duplicate filenames come from different paths', () => {
    const group = mkGroup([
      mkReadItem('r1', 'Read: /home/me/repo/internal/store/paging_test.go'),
      mkReadItem('r2', 'Read: /home/me/repo/app_paging_test.go'),
      mkReadItem('r3', 'Read: /home/me/repo/frontend/src/lib/paging_test.go'),
    ]);
    const { getAllByTestId } = render(ReadGroupRow, {
      props: { pane: paneWithWorkspace('/home/me/repo'), group },
    });

    const links = getAllByTestId('editor-link');
    expect(links.map((el) => el.textContent?.trim())).toEqual([
      'internal/store/paging_test.go',
      'app_paging_test.go',
      'frontend/src/lib/paging_test.go',
    ]);
    expect(links[0].getAttribute('title')).toBe(
      'Open internal/store/paging_test.go in editor',
    );
  });

  it('dedupes exact duplicate paths', () => {
    const group = mkGroup([
      mkReadItem('r1', 'Read: /home/me/repo/internal/store/paging_test.go'),
      mkReadItem('r2', 'Read: /home/me/repo/internal/store/paging_test.go'),
      mkReadItem('r3', 'Read: /home/me/repo/app_paging.go'),
    ]);
    const { getAllByTestId } = render(ReadGroupRow, {
      props: { pane: paneWithWorkspace('/home/me/repo'), group },
    });

    expect(getAllByTestId('editor-link').map((el) => el.textContent?.trim())).toEqual([
      'paging_test.go',
      'app_paging.go',
    ]);
  });

  it('dedupes absolute and relative forms of the same workspace path', () => {
    const group = mkGroup([
      mkReadItem('r1', 'Read: /home/me/repo/src/events.ts'),
      mkReadItem('r2', 'Read: src/events.ts'),
      mkReadItem('r3', 'Read: /home/me/repo/src/other.ts'),
    ]);
    const { getAllByTestId } = render(ReadGroupRow, {
      props: { pane: paneWithWorkspace('/home/me/repo'), group },
    });

    const links = getAllByTestId('editor-link');
    expect(links.map((el) => el.getAttribute('data-path'))).toEqual([
      'src/events.ts',
      'src/other.ts',
    ]);
    expect(links.map((el) => el.textContent?.trim())).toEqual([
      'events.ts',
      'other.ts',
    ]);
  });

  it('keeps same-file reads with different line numbers distinct', () => {
    const group = mkGroup([
      mkReadItem('r1', 'Read: /home/me/repo/src/events.ts:10'),
      mkReadItem('r2', 'Read: /home/me/repo/src/events.ts:40'),
      mkReadItem('r3', 'Read: /home/me/repo/src/events.ts:10'),
    ]);
    const { getAllByTestId } = render(ReadGroupRow, {
      props: { pane: paneWithWorkspace('/home/me/repo'), group },
    });

    const links = getAllByTestId('editor-link');
    expect(links.map((el) => el.getAttribute('data-path'))).toEqual([
      'src/events.ts',
      'src/events.ts',
    ]);
    expect(links.map((el) => el.getAttribute('aria-label'))).toEqual([
      'Open events.ts:10 in editor',
      'Open events.ts:40 in editor',
    ]);
  });

  it('renders a grayed-out chevron in the first column for alignment with disclosure rows', () => {
    const group = mkGroup([
      mkReadItem('r1', 'Read: a.go'),
      mkReadItem('r2', 'Read: b.go'),
    ]);
    const { getByTestId } = render(ReadGroupRow, {
      props: { pane: paneWithWorkspace(''), group },
    });
    const row = getByTestId('read-group-row');
    const chevronSlot = row.children[0] as HTMLElement;
    expect(chevronSlot.querySelector('svg')).not.toBeNull();
    expect(chevronSlot.className).toContain('opacity-30');
  });

  it('renders the eye tool-kind so the rail rhythm matches single Read rows', () => {
    // The continuous left rail under consecutive tool rows aligns
    // icon columns by tool kind. ReadGroupRow keeps `eye` (same as a
    // solitary Read row's classification) so swapping a single Read
    // for a run-of-two doesn't shift the icon column under it.
    const group = mkGroup([
      mkReadItem('r1', 'Read: a.go'),
      mkReadItem('r2', 'Read: b.go'),
    ]);
    const { getByTestId } = render(ReadGroupRow, {
      props: { pane: paneWithWorkspace(''), group },
    });
    expect(getByTestId('read-group-row').getAttribute('data-tool-kind')).toBe('eye');
  });
});
