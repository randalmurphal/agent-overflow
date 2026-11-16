// Timeline rows of each kind that shows find text, as the store serves
// them: threadFindText.browser.test.ts renders each, holds the text it
// marks to rowFindRuns, and holds findCandidateVectors.json, which the
// store's candidate test reads, to the runs they project.

import type { Item } from '../../lib/types/models';
import { CORPUS, findCandidateCombinations, generatedMarkdown } from './markdownFindCorpus';
import agentMessages from '../../../../internal/app/testdata/agent_message_labels.json';

export const FIND_TEXT_THREAD = 'thread-1';
/** The workspace of makeThread, which the rows' paths are shown against. */
export const FIND_TEXT_WORKSPACE = '/tmp/workspace';

export interface FindTextRow {
  name: string;
  item: Item;
  /** A reasoning row's expanded body: the payload data the reader loaded. */
  expanded?: string;
}

let next = 0;
function row(name: string, fields: Partial<Item> & Pick<Item, 'kind' | 'summary'>, expanded?: string): FindTextRow {
  next += 1;
  return {
    name,
    expanded,
    item: {
      id: `row-${next}`,
      threadId: FIND_TEXT_THREAD,
      turnIndex: next,
      itemIndex: 0,
      role: fields.kind === 'user_text' ? 'user' : 'assistant',
      status: 'completed',
      meta: '{}',
      createdAt: 1_000 * next,
      updatedAt: 1_000 * next,
      rev: 0,
      ...fields,
    },
  };
}

const bash = (input: Record<string, unknown>) => JSON.stringify({ input });

/** The name prefix of a row made from Go's agent message labels fixture. */
export const AGENT_MESSAGE_ROW = 'agent message: ';

/**
 * An agent's message as a client receives it, with the facts Go serves
 * (internal/app/testdata/agent_message_labels.json).
 */
function agentMessageRow(name: string): FindTextRow {
  const message = agentMessages.find((candidate) => candidate.name === name);
  if (!message) throw new Error(`no agent message ${name}`);
  return row(`${AGENT_MESSAGE_ROW}${name}`, { kind: 'user_text', summary: message.text, meta: message.meta });
}

export const FIND_TEXT_ROWS: FindTextRow[] = [
  row('user plain', { kind: 'user_text', summary: 'Please find **needle** and [docs](https://example.com/x) as typed' }),
  row('user with an image block', { kind: 'user_text', summary: 'look at this needle\n\n![image](attachment://att-1)' }),
  row('user with an image block between messages', {
    kind: 'user_text',
    summary: 'see [docs](https://example.com/x) needle\n\n![image](attachment://att-1)\n\n---\n\nsecond needle',
  }),
  // The bubble drops the typed command its block shows, joining the parts around it.
  row('user with a command between messages', {
    kind: 'user_text',
    summary: 'first needle part\n\n---\n\n!ls -la\n\n---\n\nsecond needle part',
    meta: JSON.stringify({
      attachments: [{
        id: 'att-shell', threadId: FIND_TEXT_THREAD, filename: 'output.txt', mimeType: 'text/plain; charset=utf-8', size: 3, kind: 'file',
        shell: { command: 'ls -la', exitCode: 0, durationMs: 5, preview: 'ok\n' },
      }],
    }),
  }),
  row('user with a bang that is no command', { kind: 'user_text', summary: 'Wow! !important: the needle stays\n\n---\n\n!still typed text' }),
  // A message an agent wrote is an agent-message row; the person's message
  // a provider joined after a wake keeps the bubble, its data decoded.
  ...['a request', 'a reply', 'a digest with no text', 'a watch', 'a workflow wake whose data holds escapes'].map(agentMessageRow),
  row('user joined after a wake', {
    kind: 'user_text',
    summary: 'Loop "nightly" fired.\nnote: "caf\\u00e9 \\u003cneedle\\u003e"\n\n---\n\nmy own needle',
    meta: JSON.stringify({ origin: 'agent-wake', originWake: { kind: 'loop', label: 'nightly' }, expandComposerCommands: true }),
  }),
  row('error', { kind: 'error', summary: 'Request failed: needle not found' }),
  row('command from meta', { kind: 'tool_call', toolName: 'Bash', summary: 'Bash', meta: bash({ command: 'grep -rn needle src/' }) }),
  row('command with a description', { kind: 'tool_call', toolName: 'Bash', summary: 'Bash', meta: bash({ command: 'ls -la', description: 'List the needle files' }) }),
  row('command in a shell wrapper', { kind: 'tool_call', toolName: 'Bash', summary: 'Bash', meta: bash({ command: "bash -lc 'make test-needle'" }) }),
  row('command from payload meta', {
    kind: 'tool_call', toolName: 'Bash', summary: 'Bash', meta: '{}',
    payloadId: 'pay-cmd', payloadKind: 'command_output', payloadMeta: JSON.stringify({ command: 'cargo test needle_case', lineCount: 1 }),
  }),
  row('generic tool argument', { kind: 'tool_call', toolName: 'WebSearch', summary: 'needle in a haystack', meta: bash({ query: 'needle in a haystack' }) }),
  row('generic tool path', { kind: 'tool_call', toolName: 'Read', summary: 'Read', meta: bash({ file_path: '/tmp/workspace/src/needle.ts' }) }),
  row('thinking collapsed', { kind: 'thinking', summary: 'the tail of a needle thought', payloadId: 'pay-think-1', payloadKind: 'thinking' }),
  row('thinking expanded', { kind: 'thinking', summary: 'the tail of it', payloadId: 'pay-think-2', payloadKind: 'thinking' },
    'An opening needle far above the tail, then the tail of it'),
  row('compaction reasoning expanded', { kind: 'compaction_reasoning', summary: 'compact tail', payloadId: 'pay-compact', payloadKind: 'thinking' },
    'Summarizing the needle work so far, ending in the compact tail'),
  row('notification', { kind: 'notification', summary: 'needle notification' }),
  ...[
    ...CORPUS,
    ...Array.from({ length: 30 }, (_, i) => generatedMarkdown(i + 101)),
    '{"answer":"raw json needle","items":[1,2]}',
  ].map((summary, at) => row(`assistant ${at}`, { kind: 'assistant_text', summary })),
];

/** The rows findCandidateVectors.json holds: every row above, every agent
 * message Go's labels fixture holds, as served, and the combinations of
 * Markdown pieces, which the vectors hold without a render. */
export const FIND_CANDIDATE_ROWS: FindTextRow[] = [
  ...FIND_TEXT_ROWS,
  ...agentMessages
    .filter((message) => !FIND_TEXT_ROWS.some((rendered) => rendered.name === `${AGENT_MESSAGE_ROW}${message.name}`))
    .map((message) => agentMessageRow(message.name)),
  ...findCandidateCombinations().map((summary, at) => row(`combination ${at}`, { kind: 'assistant_text', summary })),
];
