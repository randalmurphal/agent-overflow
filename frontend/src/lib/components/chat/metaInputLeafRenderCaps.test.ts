// Tripwire for the assumption the wire projection rests on
// (internal/itemwire): every surface that renders a `meta.input` leaf
// caps its own output far below the projection's leaf floor, so no leaf
// small enough to be rendered whole is ever an elision candidate — and
// the two readers that DON'T cap are named in the projection's retain
// set rather than trusted.
//
// That is a claim about frontend code held by a Go constant, which is
// the kind of pair that decays silently. This file is the failure mode:
// a new uncapped reader of a `meta.input` leaf, or a cap raised past the
// floor, fails here rather than shipping a row that renders short.
//
// It is a size property, not a snapshot — nothing here asserts exact
// preview text, so ordinary copy changes do not touch it.

import { describe, expect, it } from 'vitest';
import { makeItem } from '../../../test/helpers/chat';
import type { Item } from '../../types/models';
import { presentToolCardInputPreview, toolCardInputPreview } from './toolCardPreview';
import { commandTextForItem } from './commandDisplay';
import { collabInputFromMeta, previewText } from './collabToolRowData';
import { extractQuestions, headerLabelForQuestions } from './askUserQuestionData';

/**
 * Mirrors `itemwire.LeafFloorBytes`. The projection never considers a
 * `meta.input` leaf smaller than this, so any reader whose output stays
 * under it can only ever have been handed a value it would have
 * truncated itself.
 */
const LEAF_FLOOR_BYTES = 1024;

/** Comfortably over the floor: a value the projection WOULD drop. */
const OVERSIZED = 'x'.repeat(8 * 1024);

function metaWithInput(input: Record<string, unknown>): string {
  return JSON.stringify({ input });
}

function toolItem(toolName: string, input: Record<string, unknown>): Item {
  return makeItem({
    kind: 'tool_call',
    toolName,
    summary: '',
    meta: metaWithInput(input),
  });
}

function previewFor(toolName: string, input: Record<string, unknown>): string {
  const item = toolItem(toolName, input);
  const itemMeta = JSON.parse(item.meta!) as Record<string, unknown>;
  return presentToolCardInputPreview(item, null, itemMeta, '/workspace').text;
}

describe('every rendered meta.input leaf is capped below the projection floor', () => {
  // Each case is one reader that pulls a leaf out of `meta.input` and
  // puts it on screen. The input is oversized in the leaf that reader
  // reads; what is asserted is only that the output stayed small.
  const cases: Array<{ reader: string; render: () => string }> = [
    {
      reader: 'MCP argument list (formatMcpArgs)',
      render: () => previewFor('mcp__server__tool', { query: OVERSIZED, filter: OVERSIZED }),
    },
    {
      reader: 'wait_agent preview',
      render: () => previewFor('wait_agent', { agent_ids: [OVERSIZED], timeout: OVERSIZED }),
    },
    {
      reader: 'collab row preview (previewText)',
      render: () => {
        const input = collabInputFromMeta({ input: { prompt: OVERSIZED } }, null);
        return previewText(String(input.prompt ?? ''));
      },
    },
  ];

  for (const { reader, render } of cases) {
    it(`${reader} renders below the ${LEAF_FLOOR_BYTES}-byte leaf floor`, () => {
      expect(render().length).toBeLessThan(LEAF_FLOOR_BYTES);
    });
  }

  // The summary fallback is the widest path through the preview: when no
  // structured reader claims the row, the card renders `item.summary`,
  // which triage caps at 80 runes before it is ever persisted. It never
  // reaches into `meta.input` at all, which is why an oversized input
  // cannot reach the screen through it.
  it('falls back to the persisted summary rather than to a raw input leaf', () => {
    const item = makeItem({
      kind: 'tool_call',
      toolName: 'UnknownTool',
      summary: 'ran the unknown tool',
      meta: metaWithInput({ payload: OVERSIZED }),
    });
    const itemMeta = JSON.parse(item.meta!) as Record<string, unknown>;
    expect(toolCardInputPreview(item, null, itemMeta)).toBe('ran the unknown tool');
  });
});

describe('the readers that do NOT cap read the identity keys the projection retains', () => {
  // The other half of the claim, and the half that decays: each case
  // renders a `meta.input` leaf in FULL, and every key it reads is in
  // `retainedIdentityKeys` (internal/itemwire/project.go). A new
  // uncapped reader is a new case here AND a new retained key — the
  // pairing is what stops one being done without the other.
  //
  // These assert the output is oversized on purpose. An assertion that
  // "fails" by the reader acquiring a cap is the signal to move the case
  // up to the capped group and drop its key from the retained set.

  const identityReaders: Array<{ reader: string; keys: string[]; render: () => string }> = [
    {
      reader: 'Skill preview',
      keys: ['skill'],
      render: () => previewFor('Skill', { skill: OVERSIZED }),
    },
    {
      reader: 'ToolSearch preview',
      keys: ['query'],
      render: () => previewFor('ToolSearch', { query: OVERSIZED }),
    },
    {
      reader: 'SendMessage recipient',
      keys: ['recipient', 'to'],
      render: () => previewFor('SendMessage', { recipient: OVERSIZED }),
    },
    {
      reader: 'structured path target (Read)',
      keys: ['file_path', 'notebook_path', 'path'],
      render: () => previewFor('Read', { file_path: OVERSIZED }),
    },
    {
      reader: 'structured file-edit path (Write)',
      keys: ['file_path', 'files'],
      render: () => previewFor('Write', { file_path: OVERSIZED, content: OVERSIZED }),
    },
  ];

  for (const { reader, keys, render } of identityReaders) {
    it(`${reader} renders ${keys.join(' / ')} whole`, () => {
      expect(render().length).toBeGreaterThan(LEAF_FLOOR_BYTES);
    });
  }

  it('commandTextForItem renders meta.input.command whole (retained: "command")', () => {
    const item = makeItem({
      kind: 'tool_call',
      toolName: 'Bash',
      meta: metaWithInput({ command: OVERSIZED }),
    });
    // Uncapped by design — the command line is the row. The projection
    // therefore drops this leaf only when `payloadMeta.command` already
    // carries the same string (itemwire.retainedCommandPath).
    expect(commandTextForItem(item, null)).toBe(OVERSIZED);
  });

  it('the question card renders meta.input.questions whole (retained: "questions")', () => {
    const meta = {
      input: {
        questions: [{ header: 'Approach', question: OVERSIZED }],
      },
    };
    const questions = extractQuestions(meta);
    expect(questions).toHaveLength(1);
    expect(questions[0].question).toBe(OVERSIZED);
    // And straight into the row header, uncapped.
    expect(headerLabelForQuestions(questions).length).toBeGreaterThan(LEAF_FLOOR_BYTES);
  });

  it('drops the question entirely if its text is missing, rather than rendering it short', () => {
    // Why "questions" is retained unconditionally while "command" is
    // retained only without a second copy: a missing command falls back
    // to the row summary, a missing question string fails the extractor
    // and the question disappears from the card.
    expect(extractQuestions({ input: { questions: [{ header: 'Approach' }] } })).toHaveLength(0);
  });
});
