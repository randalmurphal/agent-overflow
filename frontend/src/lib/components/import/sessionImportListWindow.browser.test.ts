import { mount, unmount } from 'svelte';
import { afterEach, describe, expect, it } from 'vitest';
import '../../../app.css';
import SessionImportList from './SessionImportList.svelte';
import type { ImportableSession } from '../../types/sessionImport';
import { waitFor } from '../../../test/helpers/browserFrames';

// The catalogue mounts only the rows in view. In real Chromium: the row the
// listbox's `aria-activedescendant` names stays in the document when the
// reader scrolls it out of the window.

const ROW_COUNT = 500;
const ID_PREFIX = 'import-row';

const ROWS: ImportableSession[] = Array.from({ length: ROW_COUNT }, (_, index) => ({
  id: `claude:${index}`,
  provider: 'claude',
  sessionId: `s${index}`,
  title: `Session ${index}`,
  projectPath: '/repos/alpha',
  projectId: 'p-alpha',
  projectLabel: 'alpha',
  createdAt: 1,
  lastActivityAt: 1,
  sizeBytes: 2048,
  subagentCount: 0,
  sourcePath: `/home/u/.claude/s${index}.jsonl`,
  knownProject: true,
  origin: '',
  ranInAgentOverflow: false,
}));

let host: HTMLDivElement | undefined;
let app: Record<string, unknown> | undefined;

afterEach(() => {
  if (app) void unmount(app);
  host?.remove();
  app = undefined;
  host = undefined;
});

describe('<SessionImportList> windowing in a real browser', () => {
  it('keeps the active row mounted when it scrolls out of the window', async () => {
    host = document.createElement('div');
    host.style.cssText = 'width: 900px;';
    document.body.appendChild(host);
    const activeDescendant = `${ID_PREFIX}-${ROWS[2].id}`;
    app = mount(SessionImportList, {
      target: host,
      props: {
        id: 'import-list',
        rows: ROWS,
        selection: new Set<string>(),
        activeIndex: 2,
        activeDescendant,
        resultFor: () => undefined,
        disabled: false,
        idPrefix: ID_PREFIX,
        onToggle: () => {},
      },
    });
    const list = host.querySelector<HTMLElement>('[data-testid="virtual-list-viewport"]')!;
    const mountedIds = () => [...host!.querySelectorAll<HTMLElement>('[role="option"]')].map((row) => row.id);
    await waitFor(() => mountedIds().length > 10, 'the viewport height to reach the list');

    list.scrollTop = list.scrollHeight;
    await waitFor(
      () => mountedIds().includes(`${ID_PREFIX}-${ROWS[ROW_COUNT - 1].id}`),
      'the last row to mount',
    );
    expect(mountedIds()).not.toContain(`${ID_PREFIX}-${ROWS[1].id}`);
    const named = document.getElementById(activeDescendant);
    expect(named).not.toBeNull();
    expect(host.querySelector('[role="listbox"]')!.getAttribute('aria-activedescendant')).toBe(activeDescendant);
    // At its own place: the third 44px row.
    const top = named!.getBoundingClientRect().top - list.getBoundingClientRect().top + list.scrollTop;
    expect(top).toBeCloseTo(2 * 44, 3);
  });
});
