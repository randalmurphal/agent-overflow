// The first-party markdown pipeline, proven at the composite level.
//
// The 2026-08-30 markdown-first-party campaign replaced the vendored
// svelte-streamdown + marked dependency with first-party source
// (frontend/src/lib/markdown/), upgraded katex to 0.18.4 and mermaid to
// 11.17.2, and rebuilt the footnote popup on the app's own Popover. The
// unit and browser suites pin the parser and each host in isolation;
// what only this level proves is the assembled path: the real wire
// streaming multi-delta chunks that split MID-construct (mid-table-row,
// mid-fence, mid-math), the store's reveal drain, the incremental
// parser, and the real SPA's lazy katex/mermaid chunks rendering into
// the timeline — with zero page errors across the whole turn.
//
// Also pinned here because no lower level can: the path-relative URL
// security boundary as the USER sees it (a `/`-leading or `//host` href
// must never render a raw anchor in the real app), and the app-level
// footnote popup including chained-reference navigation, which spans
// the renderer seam, the document-source registry, and the singleton
// host — three units that only meet in the running app.
import type { Page } from '@playwright/test';
import { test, expect } from './fixtures.js';
import {
  RESULT_LINE,
  claudeScenario,
  emit,
  seedAgentThread,
  startMock,
} from './agent-visibility-helpers.js';

const j = (value: unknown): string => JSON.stringify(value);

/**
 * Streamed assistant text as MANY content_block_delta lines — the shape a
 * real provider produces — so the incremental parser sees the document
 * grow through unstable intermediate states instead of arriving whole.
 */
function multiDeltaTextLines(messageId: string, chunks: string[]): string[] {
  const full = chunks.join('');
  return [
    j({ type: 'stream_event', event: 'message_start', data: { type: 'message_start', message: { id: messageId, role: 'assistant' } } }),
    j({ type: 'stream_event', event: 'content_block_start', data: { type: 'content_block_start', index: 0, content_block: { type: 'text', text: '' } } }),
    ...chunks.map((text) =>
      j({ type: 'stream_event', event: 'content_block_delta', data: { type: 'content_block_delta', delta: { type: 'text_delta', text } } }),
    ),
    j({ type: 'stream_event', event: 'content_block_stop', data: { type: 'content_block_stop', index: 0 } }),
    j({ type: 'stream_event', event: 'message_stop', data: { type: 'message_stop' } }),
    j({ type: 'assistant', message: { id: messageId, role: 'assistant', model: 'claude-mock-1', content: [{ type: 'text', text: full }] } }),
  ];
}

// One document covering every risky surface, split at deliberately nasty
// boundaries: mid-word, mid-table-row, mid-math, mid-mermaid-fence,
// mid-link, mid-footnote-definition.
const DOC_CHUNKS = [
  '## Rendered check\n\nInline math $E=m',
  'c^2$ inside prose, then a table:\n\n| left | right |\n| --- | -',
  '-- |\n| alpha | beta |\n\n$$\\int_0^1 x^2\\,dx = \\frac',
  '{1}{3}$$\n\n```mermaid\ngraph TD\n  A[Start] --',
  '> B[Finish]\n```\n\nA safe [absolute link](https://example.com/docs), a ',
  '[path link](/etc/passwd), and a [network link](//evil.example/x).\n\n',
  'A claim[^n1] with a second marker.[^n2]\n\n[^n1]: First note referencing',
  '[^n2] for the chained hop.\n[^n2]: The chained body text.\n',
];

function watchPageErrors(page: Page): string[] {
  const errors: string[] = [];
  page.on('pageerror', (err) => errors.push(String(err)));
  return errors;
}

test('a multi-delta stream renders math, mermaid, tables, link policy, and footnote chips without a page error', async ({ harness, page }) => {
  await harness.rpc('HarnessSetScenario', {
    scenario: claudeScenario('markdown-rich-doc', [
      emit([...multiDeltaTextLines('msg-md', DOC_CHUNKS), RESULT_LINE]),
    ]),
  });
  const threadId = await seedAgentThread(harness, 'markdown-render-app', 'Markdown check');
  await harness.open(page);
  const pageErrors = watchPageErrors(page);
  await page.getByText('Markdown check').click();
  await startMock(harness, threadId);
  await harness.rpc('SendMessage', threadId, 'render everything', null);
  await harness.waitForEvent('provider:turn_completed');

  const timeline = page.getByTestId('message-timeline-scroll');

  // The mermaid SVG doubles as the settle barrier: the diagram host only
  // replaces its deferred fallback once the reveal drain finishes and the
  // row leaves streaming mode, so waiting for it first keeps every
  // assertion below post-settle. (This is also the regression the compact
  // static path had: a warm span cache serialized the settled fence as a
  // plain code block and no SVG ever appeared —
  // ChatMarkdown.compactStaticMermaid.test.ts pins the unit.)
  const mermaidHost = timeline.locator('.streamdown-mermaid-host');
  await expect(mermaidHost.locator('svg').first()).toBeVisible({ timeout: 30_000 });
  await expect(mermaidHost).toHaveCount(1);
  await expect(mermaidHost).toContainText('Start');
  await expect(mermaidHost).toContainText('Finish');

  // Prose + table content fully revealed.
  await expect(timeline.getByText('inside prose, then a table')).toBeVisible({ timeout: 30_000 });
  await expect(timeline.getByRole('cell', { name: 'alpha' })).toBeVisible();
  await expect(timeline.getByRole('cell', { name: 'beta' })).toBeVisible();

  // KaTeX (0.18.4, lazy chunk): both math hosts resolve to real .katex
  // output — an empty host or the raw source text means the async load
  // or the render threw.
  await expect(timeline.locator('[data-math-source] .katex').first()).toBeVisible({ timeout: 30_000 });
  await expect(timeline.locator('[data-math-source]')).toHaveCount(2);
  await expect(timeline.locator('[data-math-source] .katex')).toHaveCount(2);

  // URL policy: the absolute https link is a real anchor. A path-shaped
  // href never renders a RAW anchor (security boundary —
  // markdown/AGENTS.md, remote-access-boundaries.md): on this surface it
  // is rewritten during parsing into a nonce'd `agent-overflow:open`
  // editor link (click-gated by editor.ResolvePath), so the only anchor
  // mentioning the path must carry that scheme, and no anchor may keep a
  // raw same-origin (`/...`) or protocol-relative (`//host`) href.
  await expect(timeline.locator('a[href="https://example.com/docs"]')).toHaveCount(1);
  const pathAnchor = timeline.locator('a[href*="passwd"]');
  await expect(pathAnchor).toHaveCount(1);
  await expect(pathAnchor).toHaveAttribute('href', /^agent-overflow:open\?nonce=/);
  await expect(timeline.locator('a[href^="/"], a[href^="//"]')).toHaveCount(0);
  await expect(timeline.locator('a[href*="evil.example"]')).toHaveCount(0);
  await expect(timeline.getByText('network link')).toBeVisible();

  // Footnotes: two reference chips in the prose; the definitions render
  // no block of their own.
  await expect(timeline.locator('[data-streamdown-footnote-ref]')).toHaveCount(2);
  await expect(timeline.getByText('The chained body text.')).toHaveCount(0);

  expect(pageErrors).toEqual([]);
});

test('the footnote popup opens, navigates a chained reference, and closes from its chip', async ({ harness, page }) => {
  await harness.rpc('HarnessSetScenario', {
    scenario: claudeScenario('markdown-footnotes', [
      emit([...multiDeltaTextLines('msg-fn', DOC_CHUNKS), RESULT_LINE]),
    ]),
  });
  const threadId = await seedAgentThread(harness, 'markdown-footnote-app', 'Footnote check');
  await harness.open(page);
  const pageErrors = watchPageErrors(page);
  await page.getByText('Footnote check').click();
  await startMock(harness, threadId);
  await harness.rpc('SendMessage', threadId, 'render footnotes', null);
  await harness.waitForEvent('provider:turn_completed');

  const timeline = page.getByTestId('message-timeline-scroll');
  // Settle barrier (see the first test): the popup resolves the body on
  // the click and never re-resolves, so clicking before the reveal drain
  // finishes snapshots a mid-drain definition.
  await expect(
    timeline.locator('.streamdown-mermaid-host svg').first(),
  ).toBeVisible({ timeout: 30_000 });
  const chips = timeline.locator('[data-streamdown-footnote-ref]');
  await expect(chips).toHaveCount(2, { timeout: 30_000 });

  // Open n1: the popup shows its body, resolved from the document source
  // on the click (the definition renders nowhere in the timeline).
  const n1 = chips.filter({ hasText: 'n1' }).first();
  await n1.click();
  const popup = page.locator('[data-footnote-popover]');
  await expect(popup).toBeVisible({ timeout: 10_000 });
  await expect(popup).toContainText('First note referencing');
  await expect(n1).toHaveAttribute('aria-expanded', 'true');

  // The [^n2] chip INSIDE the popup navigates the popup in place: its
  // nearest markdown surface is the popup's own (whose source is just
  // the body on display), so the chained lookup must reach back to the
  // original document root. Same popup, same anchor, new body.
  const chained = popup.locator('[data-streamdown-footnote-ref]');
  await expect(chained).toHaveCount(1);
  await chained.click();
  await expect(popup).toContainText('The chained body text.', { timeout: 10_000 });
  await expect(n1).toHaveAttribute('aria-expanded', 'true');

  // A second click on the anchor chip closes the popup (the toggle runs
  // before body resolution, so it holds even if the definition is gone).
  await n1.click();
  await expect(popup).toHaveCount(0);

  expect(pageErrors).toEqual([]);
});
