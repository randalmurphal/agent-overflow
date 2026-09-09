import { describe, expect, it } from 'vitest';
import type { Item } from '../types/models';
import { subagentExecutionItem } from './codexSubagentRuntime';
const launch: Item = {
 id: 'spawn', threadId: 'thread', turnIndex: 0, itemIndex: 0,
 kind: 'tool_call', role: 'assistant', status: 'completed', toolName: 'collab_agent',
 summary: 'Worker', isBackground: true, createdAt: 100, updatedAt: 150,
 meta: JSON.stringify({input: {tool: 'spawn_agent', receiverThreadIds: ['child']},
 codex_live_projection: true, live_background_active: true, codex_runtime: {turnId: 'B', status: 'running', startedAt: 300, updatedAt: 310}}),
};
const oldAnswer: Item = {...launch, id: 'answer-A', kind: 'tool_completion', completionOf: 'spawn', createdAt: 200, updatedAt: 400};
describe('Codex execution views', () => {
 it('never treats historical spawn metadata as live state', () => {
  const meta = JSON.parse(launch.meta!);
  delete meta.codex_live_projection;
  const historical = {...launch, meta: JSON.stringify(meta)};
  expect(subagentExecutionItem(historical, oldAnswer)).toBe(oldAnswer);
 });
 it('keeps a newer execution running despite an older answer', () => {
  const view = subagentExecutionItem(launch, oldAnswer);
  expect(view.status).toBe('running');
  expect(view.createdAt).toBe(300);
  expect(launch.status).toBe('completed');
  expect(oldAnswer.status).toBe('completed');
 });
 it('settles from child lifecycle before answer delivery', () => {
  const meta = JSON.parse(launch.meta!);
  meta.live_background_active = false;
  meta.codex_runtime.status = 'interrupted';
  expect(subagentExecutionItem({...launch, meta: JSON.stringify(meta)}).status).toBe('killed');
 });
 it('stops spinning after disconnect', () => {
  const meta = {...JSON.parse(launch.meta!), live_background_active: false, codex_background_end_reason: 'session_ended'};
  expect(subagentExecutionItem({...launch, meta: JSON.stringify(meta)}).status).toBe('killed');
 });
 it('preserves Claude completion semantics', () => {
  expect(subagentExecutionItem({...launch, toolName: 'Agent'}, oldAnswer)).toBe(oldAnswer);
 });
});
