// Every AO tool's gutter verb shows whole in the header's fixed label
// column, desktop and compact, in the shipped font: the column does not
// widen for a verb, so a verb is chosen to fit it.

import { afterEach, beforeEach, expect, it } from 'vitest';
import { mount, tick, unmount } from 'svelte';
import '../../../app.css';
import GenericToolCallRow from './GenericToolCallRow.svelte';
import { makeItem } from '../../../test/helpers/chat';
import { setCompactLayoutForTest } from '../../stores/layoutMode.svelte';
import { setBindingMock } from '../../../test/mocks/bindings-app';
import { AO_TOOL_SERVERS } from './aoTools';

let host: HTMLDivElement;
beforeEach(() => {
  host = document.createElement('div');
  host.style.width = '412px';
  document.body.append(host);
  setBindingMock('ListThreadRemoteCommands', async () => []);
});
afterEach(() => {
  host.remove();
  setCompactLayoutForTest(false);
});

async function clippedVerbs(): Promise<string[]> {
  const clipped: string[] = [];
  for (const [server, spec] of Object.entries(AO_TOOL_SERVERS)) {
    for (const [tool, { label }] of Object.entries(spec.tools)) {
      const item = makeItem({
        id: `tool-${tool}`,
        threadId: 'thread-labels',
        kind: 'tool_call',
        status: 'completed',
        toolName: `MCP/${tool}`,
        summary: `MCP/${tool}`,
        meta: JSON.stringify({ mcp: { server, tool }, input: {} }),
      });
      const row = mount(GenericToolCallRow, { target: host, props: { item } });
      await tick();
      const slot = host.querySelector('[data-testid="tool-call-card-toggle-label-slot"]') as HTMLElement;
      expect(slot.textContent?.trim()).toBe(label);
      if (slot.scrollWidth > slot.clientWidth) clipped.push(`${tool}: ${label}`);
      await unmount(row);
    }
  }
  return clipped;
}

it('shows every AO verb whole in the desktop label column', async () => {
  expect(await clippedVerbs()).toEqual([]);
});

it('shows every AO verb whole in the compact label column', async () => {
  setCompactLayoutForTest(true);
  expect(document.querySelector('.layout-compact')).not.toBeNull();
  expect(await clippedVerbs()).toEqual([]);
});
