import { beforeAll, beforeEach, describe, expect, it } from 'vitest';
import { fireEvent, render } from '@testing-library/svelte';
import PlanFollowUpBanner from './PlanFollowUpBanner.svelte';
import { createComposerDraftStore } from '../../stores/composerDraft.svelte';
import { buildPane, makeItem } from '../../../test/helpers/chat';
import { resetBindingMocks, setBindingMock } from '../../../test/mocks/bindings-app';
import { installAnimateShim } from '../../../test/integration/_helpers';

beforeAll(installAnimateShim);

describe('<PlanFollowUpBanner>', () => {
  beforeEach(() => {
    resetBindingMocks();
    setBindingMock('GetDraft', async () => ({ content: '', attachmentIds: [], terminalChips: [] }));
    setBindingMock('ListAttachments', async () => []);
    setBindingMock('SaveDraft', async () => {});
    setBindingMock('ClearDraft', async () => {});
  });

  it('shows for the latest proposed-plan payload item and seeds the implement prompt', async () => {
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

    expect(draft.content).toBe('Please implement the plan above.');
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
