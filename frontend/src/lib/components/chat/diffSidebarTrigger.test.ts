import { describe, it, expect } from 'vitest';
import { isPromoteModifier, openDiffSidebar } from './diffSidebarTrigger';
import type { ThreadPane } from '../../stores/thread.svelte';

function makeFakePane() {
  let captured: { payloadId: string; filePath?: string } | null = null;
  const pane = {
    openDiffSidebar(payload: { payloadId: string; filePath?: string }) {
      captured = payload;
    },
  } as unknown as ThreadPane;
  return {
    pane,
    captured: () => captured,
  };
}

describe('diffSidebarTrigger', () => {
  it('forwards payloadId + filePath to pane.openDiffSidebar', () => {
    const fake = makeFakePane();
    openDiffSidebar(fake.pane, { payloadId: 'p1', filePath: 'src/foo.ts' });
    expect(fake.captured()).toEqual({ payloadId: 'p1', filePath: 'src/foo.ts' });
  });

  it('forwards payloadId without filePath when omitted', () => {
    const fake = makeFakePane();
    openDiffSidebar(fake.pane, { payloadId: 'p1' });
    expect(fake.captured()).toEqual({ payloadId: 'p1', filePath: undefined });
  });

  it('isPromoteModifier returns true for Cmd-click and Ctrl-click', () => {
    const cmd = new MouseEvent('click', { metaKey: true });
    const ctrl = new MouseEvent('click', { ctrlKey: true });
    expect(isPromoteModifier(cmd)).toBe(true);
    expect(isPromoteModifier(ctrl)).toBe(true);
  });

  it('isPromoteModifier returns false for plain click and shift-only click', () => {
    const plain = new MouseEvent('click');
    const shift = new MouseEvent('click', { shiftKey: true });
    expect(isPromoteModifier(plain)).toBe(false);
    expect(isPromoteModifier(shift)).toBe(false);
  });
});
