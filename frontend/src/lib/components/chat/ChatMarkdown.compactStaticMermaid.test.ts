// A mermaid fence must never be serialized by the compact static CODE
// path. Element.svelte routes `lang === 'mermaid'` to the diagram host
// BEFORE the code host; `renderStaticTokenHtml` serializes code tokens
// through `staticRenderers.code`, and without the mirrored routing a
// WARM span cache (the backend highlights every fence — all-plain for
// languages tree-sitter doesn't know — and both the persisted-blob
// ingest and `highlight:seed` pushes warm the frontend cache) let the
// static renderer swallow the DIAGRAM into a plain <pre><code> block.
// Silently, and only in the real app: every test environment has no
// backend, so the cache was always cold and the island always mounted.
// Found by e2e/tests/markdown-render.spec.ts against the harness.
//
// This test reproduces the warm-cache condition the suites never had.
import { render, waitFor } from '@testing-library/svelte';
import { afterEach, describe, expect, it } from 'vitest';
import ChatMarkdown from './ChatMarkdown.svelte';
import {
  createCodeSourceIdentity,
  resetCodeSpanCacheForTest,
  seedFinalBlockSpans,
} from './markdown/codeSpanCache';

const MERMAID_SOURCE = 'graph TD\n  A[Start] --> B[Finish]';
const DOC = `Before.\n\n\`\`\`mermaid\n${MERMAID_SOURCE}\n\`\`\`\n\nAfter.`;

afterEach(() => {
  resetCodeSpanCacheForTest();
});

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
