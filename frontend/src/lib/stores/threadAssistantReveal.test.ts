import { describe, expect, it, vi } from 'vitest';
import { makeItem } from '../../test/helpers/chat';
import type { Item } from '../types/models';
import { matchesProvenAppend } from '../markdown';
import { createThreadAssistantReveal } from './threadAssistantReveal.svelte';
import type {
  StreamingAssistantCommitMode,
  StreamingAssistantRenderContext,
  StreamingAssistantRevealSink,
} from './streamingAssistantReveal';

const RENDER_CONTEXT: StreamingAssistantRenderContext = {
  streaming: true,
  volatileTailVisible: true,
  pathLinksInert: false,
  workspacePath: '/workspace',
  previewKey: '',
};

function literalSink(): StreamingAssistantRevealSink {
  return {
    canAppendLiteral: () => true,
    appendLiteral: () => {},
    restoreLiteral: () => true,
    reset: () => {},
  };
}

describe('thread assistant parser source', () => {
  it('batches direct suffixes without copying the full parser checkpoint', () => {
    let item: Item = makeItem({
      id: 'assistant',
      kind: 'assistant_text',
      status: 'streaming',
      summary: '',
    });
    const reveal = createThreadAssistantReveal({
      getItemIndex: (itemId) => itemId === item.id ? 0 : undefined,
      getItems: () => [item],
      setItemAt: (index, next) => {
        if (index !== 0 || next.id !== item.id) throw new Error('wrong test item');
        item = next;
      },
      hasSmoother: () => true,
    });
    reveal.register(item.id, literalSink());

    const publish = (delta: string): StreamingAssistantCommitMode => {
      const source = item.summary;
      const previousCodeUnit = source.length === 0
        ? -1
        : source.charCodeAt(source.length - 1);
      let committedMode: StreamingAssistantCommitMode | undefined;
      reveal.publish(
        item.id,
        previousCodeUnit,
        source,
        delta,
        (nextSource, mode) => {
          committedMode = mode;
          item = { ...item, summary: nextSource };
        },
      );
      if (!committedMode) throw new Error('publish did not commit');
      return committedMode;
    };

    expect(publish('alpha')).toBe('authoritative');
    const checkpoint = reveal.parserSource(item.id, item.summary, RENDER_CONTEXT);
    expect(checkpoint).toBe('alpha');
    expect(publish(' beta')).toBe('direct');
    expect(publish(' gamma')).toBe('direct');

    const join = vi.spyOn(Array.prototype, 'join');
    expect(publish('.')).toBe('authoritative');
    // Combining pending reveal units may join those units once. Copying
    // [checkpoint, combinedDelta] would make every authoritative reveal
    // allocate the full growing answer again.
    expect(join).toHaveBeenCalledTimes(1);
    join.mockRestore();

    const parserSource = reveal.parserSource(item.id, item.summary, RENDER_CONTEXT);
    const append = reveal.sourceAppend(item.id, parserSource);
    expect(parserSource).toBe('alpha beta gamma.');
    expect(matchesProvenAppend(append, checkpoint, parserSource)).toBe(true);
  });
});
