// Every surface that renders a row's or a channel message's markdown resolves
// a relative image against the workspace it recorded
// (`meta.authoredWorkspace`), not the thread's current one: the GetLocalImage
// call names the recorded workspace.

import { afterEach, beforeAll, beforeEach, describe, expect, it } from 'vitest';
import { cleanup, fireEvent, render, waitFor } from '@testing-library/svelte';
import AdvisorRow from './AdvisorRow.svelte';
import AskUserQuestionCard from './AskUserQuestionCard.svelte';
import AssistantMessage from './AssistantMessage.svelte';
import CommandResultRow from './CommandResultRow.svelte';
import PlanSidebar from './PlanSidebar.svelte';
import ProposedPlanCard from './ProposedPlanCard.svelte';
import ChannelView from '../discussion/ChannelView.svelte';
import { buildPane, makeItem, makeThread } from '../../../test/helpers/chat';
import { makeChannelMessage, makeChannelStatePayload } from '../../../test/helpers/discussion';
import { mockLocalImage } from '../../../test/mocks/attachmentTransfer';
import { resetBindingMocks, setBindingMock } from '../../../test/mocks/bindings-app';
import { installAnimateShim } from '../../../test/integration/_helpers';
import { makePanelContext } from '../../stores/panelContext.svelte';
import { resetProposedPlanCacheForTests } from '../../stores/proposedPlans.svelte';
import type { ThreadPane } from '../../stores/thread.svelte';
import type { Item } from '../../types/models';
import { __reportImageBoxForTest, __resetImageTiersForTest } from '../../utils/imageTiers';
import { __resetMediaBlobCacheForTest } from '../../utils/mediaBlobCache';

const THREAD_WORKSPACE = '/thread-now';
const AUTHORED_WORKSPACE = '/written-in';

beforeAll(installAnimateShim);

beforeEach(() => {
  resetBindingMocks();
  resetProposedPlanCacheForTests();
  setBindingMock('ListProposedPlanComments', async () => []);
});

afterEach(() => {
  cleanup();
  resetProposedPlanCacheForTests();
  __resetImageTiersForTest();
  __resetMediaBlobCacheForTest();
});

function payload(data: string): void {
  setBindingMock('GetPayloadPreview', async () => ({
    data,
    nextOffset: data.length,
    totalSize: data.length,
    isComplete: true,
  }));
  setBindingMock('GetPayloadData', async () => ({ data }));
}

function withAuthored(meta: Record<string, unknown>, authored: boolean): string {
  return JSON.stringify(authored ? { ...meta, authoredWorkspace: AUTHORED_WORKSPACE } : meta);
}

interface Surface {
  name: string;
  image: string;
  /** Renders the surface on a pane whose thread workspace is THREAD_WORKSPACE. */
  mount(image: string, authored: boolean): Promise<void>;
}

async function paneWith(items: Item[] = []): Promise<ThreadPane> {
  return buildPane(makeThread({ workspacePath: THREAD_WORKSPACE }), items);
}

type RowComponent = Parameters<typeof render>[0];

async function mountRow(component: RowComponent, item: Item, extra: Record<string, unknown> = {}) {
  const pane = await paneWith([item]);
  return render(component, { props: { pane, item, ...extra } as never });
}

const surfaces: Surface[] = [
  {
    name: 'AssistantMessage',
    image: 'shots/assistant.png',
    async mount(image, authored) {
      await mountRow(AssistantMessage, makeItem({
        summary: `![shot](${image})`,
        meta: withAuthored({}, authored),
      }));
    },
  },
  {
    name: 'AskUserQuestionCard prompt',
    image: 'shots/question.png',
    async mount(image, authored) {
      const view = await mountRow(AskUserQuestionCard, makeItem({
        kind: 'tool_call',
        status: 'running',
        toolName: 'AskUserQuestion',
        meta: withAuthored({
          toolName: 'AskUserQuestion',
          input: { questions: [{ id: 'q', header: 'Q', question: `Pick ![shot](${image})`, options: [] }] },
        }, authored),
      }));
      await fireEvent.click(view.getByTestId('ask-user-question-toggle'));
    },
  },
  {
    name: 'AskUserQuestionCard option preview',
    image: 'shots/preview.png',
    async mount(image, authored) {
      const view = await mountRow(AskUserQuestionCard, makeItem({
        kind: 'tool_call',
        status: 'completed',
        toolName: 'AskUserQuestion',
        meta: withAuthored({
          toolName: 'AskUserQuestion',
          input: {
            questions: [{
              id: 'q',
              header: 'Q',
              question: 'Pick one',
              options: [{ label: 'A', description: '', preview: `![shot](${image})` }],
            }],
          },
          answers: { 'Pick one': 'A' },
        }, authored),
      }));
      await fireEvent.click(view.getByTestId('ask-user-question-toggle'));
    },
  },
  {
    name: 'CommandResultRow',
    image: 'shots/command.png',
    async mount(image, authored) {
      const markdown = `![shot](${image})`;
      await mountRow(CommandResultRow, makeItem({
        id: 'command-result:skill-1',
        kind: 'command_result',
        role: 'system',
        summary: markdown,
        meta: withAuthored({
          kind: 'command_result',
          preview: markdown,
          agentResult: { launchId: 'claude-command:cmd-1', sourceKind: 'skill', sourceName: 'review' },
        }, authored),
      }));
    },
  },
  {
    name: 'AdvisorRow',
    image: 'shots/advisor.png',
    async mount(image, authored) {
      payload(`![shot](${image})`);
      const view = await mountRow(AdvisorRow, makeItem({
        kind: 'tool_call',
        toolName: 'advisor',
        summary: 'advisor',
        payloadId: 'tool-call-result:advisor',
        payloadKind: 'tool_call_result',
        payloadMeta: JSON.stringify({ preview: 'advice' }),
        meta: withAuthored({ toolName: 'advisor' }, authored),
      }));
      await fireEvent.click(view.getByTestId('advisor-row-toggle'));
    },
  },
  {
    name: 'ProposedPlanCard',
    image: 'shots/plan-card.png',
    async mount(image, authored) {
      const markdown = `# Plan\n\n![shot](${image})`;
      payload(markdown);
      const meta = { title: 'Plan', lineCount: 3, charCount: markdown.length, preview: markdown };
      await mountRow(ProposedPlanCard, makeItem({
        id: 'plan',
        kind: 'tool_call',
        toolName: 'ExitPlanMode',
        payloadId: 'payload-plan',
        payloadKind: 'proposed_plan',
        payloadMeta: JSON.stringify(meta),
        meta: withAuthored({}, authored),
      }), { meta });
    },
  },
  {
    name: 'PlanSidebar',
    image: 'shots/plan-sidebar.png',
    async mount(image, authored) {
      const markdown = `# Plan\n\n![shot](${image})`;
      payload(markdown);
      const plan = makeItem({
        id: 'plan',
        kind: 'tool_call',
        toolName: 'ExitPlanMode',
        payloadId: 'payload-plan',
        payloadKind: 'proposed_plan',
        payloadMeta: JSON.stringify({ title: 'Plan', lineCount: 3, charCount: markdown.length, preview: markdown }),
        meta: withAuthored({}, authored),
      });
      const pane = await paneWith([plan]);
      pane.setShowPlanSidebar(true);
      render(PlanSidebar, { props: { ctx: makePanelContext(pane, () => {}) } });
    },
  },
  {
    name: 'ChannelView message',
    image: 'shots/channel.png',
    async mount(image, authored) {
      setBindingMock('GetChannelState', async () => makeChannelStatePayload({ participants: [] }));
      setBindingMock('GetChannelMessages', async () => [
        makeChannelMessage({ content: `![shot](${image})`, meta: withAuthored({}, authored) }),
      ]);
      const thread = makeThread({ workspacePath: THREAD_WORKSPACE, mode: 'discussion', discussionId: 'channel-1' });
      render(ChannelView, { props: { pane: await buildPane(thread), channelId: 'channel-1' } });
    },
  },
];

async function fetchedWorkspace(surface: Surface, authored: boolean): Promise<string> {
  const getLocalImage = mockLocalImage();
  await surface.mount(surface.image, authored);
  // happy-dom lays nothing out: report the image box width so the fetch starts.
  await waitFor(() => {
    __reportImageBoxForTest(800);
    expect(getLocalImage).toHaveBeenCalled();
  });
  const calls = getLocalImage.mock.calls.filter((call) => call[0] === surface.image);
  expect(calls.length, `GetLocalImage calls: ${JSON.stringify(getLocalImage.mock.calls)}`).toBeGreaterThan(0);
  const workspaces = new Set(calls.map((call) => call[1]));
  expect(workspaces.size).toBe(1);
  return [...workspaces][0] as string;
}

describe('relative markdown images resolve against the row authored workspace', () => {
  it.each(surfaces.map((surface) => [surface.name, surface] as const))(
    '%s fetches from the recorded workspace',
    async (_name, surface) => {
      expect(await fetchedWorkspace(surface, true)).toBe(AUTHORED_WORKSPACE);
    },
  );

  it('AssistantMessage fetches from the thread workspace when the row recorded none', async () => {
    expect(await fetchedWorkspace(surfaces[0]!, false)).toBe(THREAD_WORKSPACE);
  });
});
