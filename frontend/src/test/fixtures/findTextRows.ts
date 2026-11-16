// Timeline rows of each kind that shows find text, as the store serves
// them: threadFindText.browser.test.ts renders each, holds the text it
// marks to rowFindRuns, and holds findCandidateVectors.json, which the
// store's candidate test reads, to the runs they project.

import type { Item } from '../../lib/types/models';
import { CORPUS, findCandidateCombinations, generatedMarkdown } from './markdownFindCorpus';

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

export const FIND_TEXT_ROWS: FindTextRow[] = [
  row('user plain', { kind: 'user_text', summary: 'Please find **needle** and [docs](https://example.com/x) as typed' }),
  row('user with an image block', { kind: 'user_text', summary: 'look at this needle\n\n![image](attachment://att-1)' }),
  row('user with an image block between messages', {
    kind: 'user_text',
    summary: 'see [docs](https://example.com/x) needle\n\n![image](attachment://att-1)\n\n---\n\nsecond needle',
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

/** The rows findCandidateVectors.json holds: every row above, and the
 * combinations of Markdown pieces, which the vectors hold without a render. */
export const FIND_CANDIDATE_ROWS: FindTextRow[] = [
  ...FIND_TEXT_ROWS,
  ...findCandidateCombinations().map((summary, at) => row(`combination ${at}`, { kind: 'assistant_text', summary })),
];
