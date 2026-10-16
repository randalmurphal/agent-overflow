// SessionImportRow chips: counts read in the right number, and the chip
// keeps its full label for assistive tech and the tooltip in both layouts.

import { describe, expect, it } from 'vitest';
import { render } from '@testing-library/svelte';
import SessionImportRow from './SessionImportRow.svelte';
import type { ImportableSession } from '../../types/sessionImport';

function row(extra: Partial<ImportableSession> = {}): ImportableSession {
  return {
    id: 'claude:a',
    provider: 'claude',
    sessionId: 'a',
    title: 'Parser work',
    projectPath: '/repos/alpha',
    projectId: 'p-alpha',
    projectLabel: 'alpha',
    createdAt: 1,
    lastActivityAt: Date.now(),
    sizeBytes: 2048,
    subagentCount: 0,
    sourcePath: '/home/u/.claude/a.jsonl',
    knownProject: true,
    origin: '',
    ranInAgentOverflow: false,
    ...extra,
  };
}

function mount(session: ImportableSession) {
  return render(SessionImportRow, {
    props: {
      row: session,
      domId: 'row',
      selected: false,
      active: false,
      result: undefined,
      disabled: false,
      onToggle: () => {},
    },
  });
}

describe('SessionImportRow', () => {
  it.each([
    [1, '1 subagent'],
    [2, '2 subagents'],
  ])('labels %i subagent chip as %s', (count, label) => {
    const view = mount(row({ subagentCount: count }));
    const chip = view.getByTestId('session-import-subagents-claude:a');
    expect(chip.getAttribute('aria-label')).toBe(label);
    expect(chip.getAttribute('title')).toBe(label);
    expect(chip.textContent).toContain(label);
  });

  it('shows no subagent chip without subagents', () => {
    const view = mount(row());
    expect(view.queryByTestId('session-import-subagents-claude:a')).toBeNull();
  });
});
