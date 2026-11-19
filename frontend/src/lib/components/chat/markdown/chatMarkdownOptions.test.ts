import { describe, expect, it } from 'vitest';
import { markedImageAttributes, markedSoftBreak } from '../../../markdown';
import type { ForgeAttachmentSource } from '../../../utils/forgeAttachments';
import { chatMarkdownExtensions, chatMarkdownForgeOptions } from './chatMarkdownOptions';

const source = (forge: 'github' | 'gitlab'): ForgeAttachmentSource => ({
  pr: { forge, host: `${forge}.com`, namespace: 'acme', repo: 'widget', number: 4 },
  backend: 'home',
  webBase: '',
});

const names = (extensions: readonly { name: string }[] | undefined) => extensions?.map((extension) => extension.name);

describe('chatMarkdownForgeOptions', () => {
  it('adds the shared soft-break extension after the attachment claims for GitHub embedded HTML', () => {
    const { extensions } = chatMarkdownForgeOptions(source('github'), true);
    // The forge's syntax extensions sit between it and the emoji slot.
    expect(names(extensions)!.slice(0, 4)).toEqual(['forgeAttachment', 'forgeAttachmentBlock', 'forgeReference', 'softBreak']);
    expect(extensions![3]).toBe(markedSoftBreak);
    expect(names(extensions)!.at(-1)).toBe('forgeEmoji');
  });

  it('leaves soft breaks alone for GitLab, without embedded HTML, and off a PR', () => {
    expect(names(chatMarkdownForgeOptions(source('gitlab'), true).extensions)).not.toContain('softBreak');
    expect(names(chatMarkdownForgeOptions(source('github'), false).extensions)).not.toContain('softBreak');
    expect(chatMarkdownForgeOptions(null, true).extensions).toBeUndefined();
  });

  it('adds the shared image attribute extension after the attachment claims for GitLab only', () => {
    for (const html of [true, false]) {
      const { extensions } = chatMarkdownForgeOptions(source('gitlab'), html);
      expect(names(extensions)!.slice(1)).toContain('imageAttributes');
      expect(extensions![names(extensions)!.indexOf('imageAttributes')]).toBe(markedImageAttributes);
      expect(names(chatMarkdownForgeOptions(source('github'), html).extensions)).not.toContain('imageAttributes');
    }
  });

  it('puts the soft-break extension ahead of the embedded HTML pair in ChatMarkdown order', () => {
    const list = chatMarkdownExtensions({ forge: chatMarkdownForgeOptions(source('github'), true) });
    const order = names(list)!;
    expect(order.slice(0, 4)).toEqual(['forgeAttachment', 'forgeAttachmentBlock', 'forgeReference', 'softBreak']);
    expect(order.indexOf('details')).toBeGreaterThan(order.indexOf('forgeEmoji'));
    expect(order.indexOf('details')).toBeGreaterThan(order.indexOf('softBreak'));
  });
});
