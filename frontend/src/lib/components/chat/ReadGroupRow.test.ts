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
  it('renders one EditorLink per member with workspace-relative labels', () => {
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
    expect(links.map((el) => el.getAttribute('data-path'))).toEqual([
      'src/foo.ts',
      'src/bar.ts',
      'test/baz_test.ts',
    ]);
    // EditorLink renders the label as the button text. The presenter
    // strips both the `Read: ` prefix and the workspace root before
    // the row sees it, so the display matches the data-path here.
    expect(links.map((el) => el.textContent?.trim())).toEqual([
      'src/foo.ts',
      'src/bar.ts',
      'test/baz_test.ts',
    ]);
    expect(getByTestId('read-group-row-label').textContent).toBe('reads');
  });

  it('leaves paths outside the workspace absolute so EditorLink can resolve them', () => {
    // Reads of system files (rare but real — agent surveying a
    // dependency under /usr or a sibling repo) must stay openable.
    // EditorLink consumes `data-path` directly when it's absolute, so
    // pinning that value here is the regression guard.
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
