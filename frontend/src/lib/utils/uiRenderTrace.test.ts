import { afterEach, describe, expect, it } from 'vitest';
import {
  clearUiRenderTrace,
  flushUiRenderTrace,
  getUiRenderTraceRecords,
  installUiRenderTraceApi,
  isUiRenderTraceEnabled,
  recordUiTrace,
  setUiRenderTraceEnabled,
} from './uiRenderTrace';
import { getBindingMock, setBindingMock } from '../../test/mocks/bindings-app';

describe('uiRenderTrace', () => {
  afterEach(() => {
    clearUiRenderTrace();
    setUiRenderTraceEnabled(false);
    delete window.__agentOverflowUiTrace;
  });

  it('does not record while disabled', () => {
    setUiRenderTraceEnabled(false);

    recordUiTrace('chat.state', { threadId: 't1' });

    expect(getUiRenderTraceRecords()).toEqual([]);
  });

  it('records bounded snapshots when enabled', () => {
    setUiRenderTraceEnabled(true);

    recordUiTrace('chat.state', { threadId: 't1' });

    const records = getUiRenderTraceRecords();
    expect(records).toHaveLength(1);
    expect(records[0]?.label).toBe('chat.state');
    expect(records[0]?.data).toEqual({ threadId: 't1' });
    expect(records[0]?.seq).toBeGreaterThan(0);
  });

  it('flushes trace records to the backend in compact batches', async () => {
    setBindingMock('AppendUIRenderTraceBatch', async () => '/tmp/ui-render.jsonl');
    setUiRenderTraceEnabled(true);

    recordUiTrace('chat.state', { threadId: 't1' });

    const path = await flushUiRenderTrace();
    const appendTrace = getBindingMock('AppendUIRenderTraceBatch');
    const lines = appendTrace?.mock.calls[0]?.[0] as string[] | undefined;
    expect(path).toBe('/tmp/ui-render.jsonl');
    expect(lines).toHaveLength(1);
    expect(JSON.parse(lines?.[0] ?? '{}')).toMatchObject({
      label: 'chat.state',
      data: { threadId: 't1' },
    });
  });

  it('installs the dev console API', async () => {
    setBindingMock('AppendUIRenderTraceBatch', async () => '/tmp/ui-render.jsonl');
    installUiRenderTraceApi();

    window.__agentOverflowUiTrace?.enable();
    recordUiTrace('timeline.state', { itemCount: 2 });

    expect(isUiRenderTraceEnabled()).toBe(true);
    expect(window.__agentOverflowUiTrace?.recent(1)[0]?.label).toBe('timeline.state');
    expect(window.__agentOverflowUiTrace?.dump(1)).toContain('timeline.state');
    await expect(window.__agentOverflowUiTrace?.flush()).resolves.toBe('/tmp/ui-render.jsonl');

    window.__agentOverflowUiTrace?.clear();
    expect(window.__agentOverflowUiTrace?.records()).toEqual([]);

    window.__agentOverflowUiTrace?.disable();
    expect(isUiRenderTraceEnabled()).toBe(false);
  });
});
