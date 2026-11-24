// A mermaid fence must never be serialized by the compact static CODE
// path. Element.svelte routes `lang === 'mermaid'` to the diagram host
// BEFORE the code host; `renderStaticTokenHtml` serializes code tokens
// through `staticRenderers.code`, and without the mirrored routing a
// WARM span cache (the backend highlights every fence — all-plain for
// languages tree-sitter doesn't know — and both the persisted-blob
// ingest and final `highlight:live` pushes warm the frontend cache) let the
// static renderer swallow the DIAGRAM into a plain <pre><code> block.
// Silently, and only in the real app: every test environment has no
// backend, so the cache was always cold and the island always mounted.
// Found by e2e/tests/markdown-render.spec.ts against the harness.
//
// This test reproduces the warm-cache condition the suites never had.
import { render, waitFor } from '@testing-library/svelte';
import { describe, expect, it } from 'vitest';
import ChatMarkdown from './ChatMarkdown.svelte';
import ForgeAttachmentChatHarness from './ForgeAttachmentChatHarness.svelte';
import { markdownFindText } from './markdown/markdownFindText';
import type { ForgeAttachmentSource } from '../../utils/forgeAttachments';
import {
  createCodeSourceIdentity,
  seedFinalBlockSpans,
} from './markdown/codeSpanCache';

const MERMAID_SOURCE = 'graph TD\n  A[Start] --> B[Finish]';
const DOC = `Before.\n\n\`\`\`mermaid\n${MERMAID_SOURCE}\n\`\`\`\n\nAfter.`;

describe('compact static path with a warm span cache', () => {
  it('mounts the mermaid island instead of serializing the fence as code', async () => {
    // Warm the cache the way the backend does for a language the
    // highlighter doesn't know: an all-plain line list.
    const identity = createCodeSourceIdentity(MERMAID_SOURCE);
    seedFinalBlockSpans(
      'mermaid',
      identity.contentKey,
      MERMAID_SOURCE.split('\n').map(() => ({})),
    );

    const { container } = render(ChatMarkdown, { props: { source: DOC } });
    await waitFor(() => {
      expect(container.textContent).toContain('After.');
    });

    // The diagram host owns the fence; the static code shell must not.
    await waitFor(() => {
      expect(
        container.querySelector('.streamdown-mermaid-host'),
      ).not.toBeNull();
    });
    expect(container.querySelector('.streamdown-code-host')).toBeNull();
  });
});

const GITHUB: ForgeAttachmentSource = {
  pr: { forge: 'github', host: 'github.com', namespace: 'acme', repo: 'widget', number: 4 },
  backend: 'home',
  webBase: 'https://github.com/acme/widget/pull/4',
};
const GITLAB: ForgeAttachmentSource = {
  pr: { forge: 'gitlab', host: 'gitlab.com', namespace: 'group', repo: 'widget', number: 3 },
  backend: 'home',
  webBase: 'https://gitlab.com/group/widget/-/merge_requests/3',
};

// A fence's language is its info string's first word (fenceLanguage), so
// `mermaid` followed by attributes is a diagram on every surface and both
// render paths: a cold cache mounts the component path, a warm one offers
// the fence to the static code renderer first.
describe('a mermaid fence with more info string words', () => {
  const doc = `Before.\n\n\`\`\`mermaid title="flow" {1}\n${MERMAID_SOURCE}\n\`\`\`\n\nAfter.`;
  it.each([
    ['agent chat', 'cold', null],
    ['agent chat', 'warm', null],
    ['GitHub', 'cold', GITHUB],
    ['GitHub', 'warm', GITHUB],
    ['GitLab', 'cold', GITLAB],
    ['GitLab', 'warm', GITLAB],
  ] as const)('renders the diagram on %s with a %s span cache', async (_name, cache, forgeSource) => {
    if (cache === 'warm') {
      const identity = createCodeSourceIdentity(MERMAID_SOURCE);
      seedFinalBlockSpans('mermaid', identity.contentKey, MERMAID_SOURCE.split('\n').map(() => ({})));
    }
    const embeddedHtml = forgeSource !== null;
    const { container } = render(ForgeAttachmentChatHarness, { props: { source: doc, forgeSource, embeddedHtml } });
    await waitFor(() => expect(container.querySelector('.streamdown-mermaid-host')).not.toBeNull());
    expect(container.querySelector('.streamdown-code-host')).toBeNull();
    expect(markdownFindText(doc, { embeddedHtml, forgeSource }).runs).toEqual(['Before.', 'After.']);
  });
});
