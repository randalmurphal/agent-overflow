import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { mount, unmount, tick } from 'svelte';
import '../../../app.css';
import TimelineLeaf from './TimelineLeaf.svelte';
import { buildPane, makeThread } from '../../../test/helpers/chat';
import { resetBindingMocks, setBindingMock } from '../../../test/mocks/bindings-app';
import { FIND_CANDIDATE_ROWS, FIND_TEXT_ROWS, FIND_TEXT_WORKSPACE, type FindTextRow } from '../../../test/fixtures/findTextRows';
import { paneWorkspacePath } from '../../stores/threadPaneShared';
import type { ThreadPane } from '../../stores/thread.svelte';
import { findTextSegments } from '../../utils/findHighlights';
import { foldFindText } from '../../utils/findMatch';
import { FIND_TEXT_SELECTOR, rowFindRuns } from './threadFindText';

// Real Chromium: each row renders through TimelineLeaf, and the text its
// renderer marks as its find text, which the highlighter paints, is held
// to rowFindRuns, which the bar counts.

let host: HTMLDivElement;
const mounted: ReturnType<typeof mount>[] = [];

beforeEach(() => {
  resetBindingMocks();
  host = document.createElement('div');
  host.style.width = '720px';
  document.body.append(host);
  setBindingMock('AppendUIRenderTraceBatch', async () => {});  const expanded = new Map(FIND_TEXT_ROWS.filter((row) => row.expanded).map((row) => [row.item.payloadId, row.expanded!]));
  setBindingMock('GetPayloadData', async (_thread: unknown, payloadId: unknown) => ({ data: expanded.get(String(payloadId)) ?? '' }));
  setBindingMock('GetPayloadChunk', async (_thread: unknown, payloadId: unknown) => {
    const data = expanded.get(String(payloadId)) ?? '';
    return { data, offset: 0, nextOffset: data.length, totalSize: data.length, isComplete: true };
  });
});

afterEach(async () => {
  for (const app of mounted.splice(0)) await unmount(app);
  host.remove();
});

function projected(pane: ThreadPane, row: FindTextRow): readonly string[] {
  return rowFindRuns(row.item, {
    provider: pane.thread?.provider,
    workspacePath: paneWorkspacePath(pane),
    expandedItemData: (itemId, stateKey) => pane.expandedItemData(itemId, stateKey),
    liveReasoningText: (itemId) => pane.liveThinkingTailForItem(itemId),
    isItemSmoothing: (itemId) => pane.isItemSmoothing(itemId),
  });
}

async function render(row: FindTextRow): Promise<{ pane: ThreadPane; target: HTMLElement }> {
  const pane = await buildPane(makeThread({ workspacePath: FIND_TEXT_WORKSPACE }), [row.item]);
  const target = document.createElement('div');
  host.append(target);
  mounted.push(mount(TimelineLeaf, { target, props: { pane, item: row.item } }));
  await tick();
  return { pane, target };
}

// Runs as find matches them: folded, a run of whitespace one space.
const text = (run: string) => foldFindText(run).replace(/\s+/g, ' ').trim();
const painted = (target: HTMLElement) => findTextSegments(target, true, FIND_TEXT_SELECTOR).map(text).filter(Boolean);
const counted = (runs: readonly string[]) => runs.map(text).filter(Boolean);

describe('thread find text', () => {
  it.each(FIND_TEXT_ROWS.map((row) => [row.name, row] as const))('%s paints the runs it counts', async (_name, row) => {
    const { pane, target } = await render(row);
    if (row.expanded) {
      const toggle = target.querySelector<HTMLElement>('[data-testid$="-toggle"]');
      expect(toggle).not.toBeNull();
      // Collapsed, the tail is find text on neither side.
      expect(painted(target)).toEqual([]);
      expect(counted(projected(pane, row))).toEqual([]);
      toggle!.click();
      await expect.poll(() => counted(projected(pane, row))).not.toEqual([]);
    }
    await expect.poll(() => painted(target), { timeout: 3_000 }).toEqual(counted(projected(pane, row)));
  });

  it("counts and paints a command block's command line", async () => {
    const want: Record<string, string[]> = {
      'user with a command alone': ['ls -la needle-dir'],
      'user with a failed command whose text changed': ['make needle-target', 'run it again'],
      'user with a command between messages': ['ls -la', 'first needle part --- second needle part'],
    };
    for (const [name, runs] of Object.entries(want)) {
      const row = FIND_TEXT_ROWS.find((each) => each.name === name)!;
      const { pane, target } = await render(row);
      expect(counted(projected(pane, row)), name).toEqual(runs);
      await expect.poll(() => painted(target), { timeout: 3_000 }).toEqual(runs);
    }
  });

  // The store's candidate test (internal/store/find_candidate_test.go) reads
  // this file: each row as the store holds it, with the payload its
  // candidate reads (a reasoning row's content, a tool row's payload meta),
  // and the runs of find text it shows at most, expanded. `vitest -u` rewrites it.
  it('writes the find runs the store candidates must hold', async () => {
    const rows = FIND_CANDIDATE_ROWS.map((row) => ({
      name: row.name,
      kind: row.item.kind,
      summary: row.item.summary,
      meta: row.item.meta ?? '',
      toolName: row.item.toolName ?? '',
      payload: row.expanded ?? row.item.payloadMeta ?? null,
      runs: rowFindRuns(row.item, {
        provider: 'claude',
        workspacePath: FIND_TEXT_WORKSPACE,
        expandedItemData: () => (row.expanded === undefined ? undefined : row.expanded),
        liveReasoningText: () => null,
        isItemSmoothing: () => false,
      }).filter((run) => run.trim()),
    }));
    expect(rows.filter((row) => !row.runs.length && !row.name.startsWith('combination')).map((row) => row.name)).toEqual([
      'generic tool path', 'thinking collapsed', 'notification',
      // Chat drops embedded HTML: these show none of it.
      'assistant 17', 'assistant 18', 'assistant 26',
    ]);
    await expect(`${JSON.stringify({ rows }, null, 2)}\n`).toMatchFileSnapshot('../../../test/fixtures/findCandidateVectors.json');
  });

  it('holds find text for every row kind that shows some', async () => {
    const withText = new Set<string>();
    for (const row of FIND_TEXT_ROWS) {
      if (row.expanded) continue;
      const { pane } = await render(row);
      if (counted(projected(pane, row)).length) withText.add(row.item.kind);
    }
    expect([...withText].sort()).toEqual(['assistant_text', 'error', 'tool_call', 'user_text']);
  });
});
