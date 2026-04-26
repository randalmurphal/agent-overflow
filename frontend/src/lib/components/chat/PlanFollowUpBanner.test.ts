import { beforeAll, beforeEach, describe, expect, it, vi } from 'vitest';
import { fireEvent, render } from '@testing-library/svelte';
import PlanFollowUpBanner from './PlanFollowUpBanner.svelte';
import { createComposerDraftStore } from '../../stores/composerDraft.svelte';
import { buildPane, makeItem } from '../../../test/helpers/chat';
import { getBindingMock, resetBindingMocks, setBindingMock } from '../../../test/mocks/bindings-app';
import { installAnimateShim } from '../../../test/integration/_helpers';

beforeAll(installAnimateShim);

describe('<PlanFollowUpBanner>', () => {
  beforeEach(() => {
    resetBindingMocks();
    setBindingMock('GetDraft', async () => ({ content: '', attachmentIds: [], terminalChips: [] }));
    setBindingMock('ListAttachments', async () => []);
    setBindingMock('SaveDraft', async () => {});
    setBindingMock('ClearDraft', async () => {});
    setBindingMock('UpdateThreadMode', async () => ({}));
    setBindingMock('SendMessageWithOptions', async () => ({
      id: 'thread-1',
      title: 'Test thread',
      provider: 'claude',
      workspacePath: '/tmp/workspace',
      projectPath: '/tmp/workspace',
      mode: 'chat',
      model: 'claude-sonnet-4-6',
      createdAt: 0,
      updatedAt: 0,
      archived: false,
    }));
  });

  it('shows for the latest proposed-plan payload item and sends the implement prompt', async () => {
    const pane = await buildPane(undefined, [
      makeItem({
        id: 'plan-1',
        kind: 'tool_call',
        payloadId: 'plan-payload',
        payloadKind: 'proposed_plan',
        payloadMeta: JSON.stringify({
          title: 'Plan',
          preview: 'plan preview',
          lineCount: 1,
          charCount: 12,
        }),
      }),
    ]);
    const draft = createComposerDraftStore();

    const { getByTestId } = render(PlanFollowUpBanner, { props: { pane, draft } });
    await fireEvent.click(getByTestId('plan-followup-implement'));

    expect(draft.content).toBe('');
    expect(getBindingMock('SendMessageWithOptions')!.mock.calls[0]).toEqual([
      'thread-1',
      'Implement the plan.',
      expect.objectContaining({
        attachmentIds: [],
        sourceProposedPlan: expect.objectContaining({
          threadId: 'thread-1',
          itemId: 'plan-1',
          payloadId: 'plan-payload',
          title: 'Plan',
        }),
      }),
    ]);
  });

  it('switches plan-mode threads back to chat when implementing', async () => {
    const thread = {
      id: 'thread-1',
      title: 'Test thread',
      provider: 'claude' as const,
      workspacePath: '/tmp/workspace',
      projectPath: '/tmp/workspace',
      mode: 'plan' as const,
      model: 'claude-sonnet-4-6',
      createdAt: 0,
      updatedAt: 0,
      archived: false,
    };
    const updated = { ...thread, mode: 'chat' as const };
    const pane = await buildPane(thread, [
      makeItem({
        id: 'plan-1',
        kind: 'tool_call',
        payloadId: 'plan-payload',
        payloadKind: 'proposed_plan',
      }),
    ]);
    const draft = createComposerDraftStore();
    setBindingMock('UpdateThreadMode', async () => updated);

    const { getByTestId } = render(PlanFollowUpBanner, { props: { pane, draft } });
    await fireEvent.click(getByTestId('plan-followup-implement'));

    expect(getBindingMock('UpdateThreadMode')!.mock.calls[0]).toEqual(['thread-1', 'chat']);
    expect(pane.thread?.mode).toBe('chat');
  });

  it('hides after dismissing the latest plan item', async () => {
    const pane = await buildPane(undefined, [
      makeItem({
        id: 'plan-1',
        kind: 'tool_call',
        payloadId: 'plan-payload',
        payloadKind: 'proposed_plan',
        payloadMeta: JSON.stringify({
          title: 'Plan',
          preview: 'plan preview',
          lineCount: 1,
          charCount: 12,
        }),
      }),
    ]);
    const draft = createComposerDraftStore();

    const { getByTestId, queryByTestId } = render(PlanFollowUpBanner, { props: { pane, draft } });
    await fireEvent.click(getByTestId('plan-followup-dismiss'));

    expect(queryByTestId('plan-followup-banner')).toBeNull();
  });

  it('clicking Review publishes a scroll-to-item request for the plan', async () => {
    const pane = await buildPane(undefined, [
      makeItem({
        id: 'plan-review',
        kind: 'tool_call',
        payloadId: 'plan-payload',
        payloadKind: 'proposed_plan',
        payloadMeta: JSON.stringify({
          title: 'Plan',
          preview: 'preview',
          lineCount: 1,
          charCount: 7,
        }),
      }),
    ]);
    const draft = createComposerDraftStore();
    const spy = vi.spyOn(pane, 'requestScrollToItem');

    const { getByTestId } = render(PlanFollowUpBanner, { props: { pane, draft } });
    await fireEvent.click(getByTestId('plan-followup-review'));

    expect(spy).toHaveBeenCalledWith('plan-review');
  });

  it('stays hidden when a newer non-plan item has landed', async () => {
    const pane = await buildPane(undefined, [
      makeItem({
        id: 'plan-1',
        kind: 'tool_call',
        payloadId: 'plan-payload',
        payloadKind: 'proposed_plan',
        payloadMeta: JSON.stringify({
          title: 'Plan',
          preview: 'plan preview',
          lineCount: 1,
          charCount: 12,
        }),
      }),
      makeItem({
        id: 'text:1:0',
        turnIndex: 1,
        itemIndex: 0,
        kind: 'assistant_text',
        summary: 'moved on',
      }),
    ]);
    const draft = createComposerDraftStore();

    const { queryByTestId } = render(PlanFollowUpBanner, { props: { pane, draft } });

    expect(queryByTestId('plan-followup-banner')).toBeNull();
  });
});
