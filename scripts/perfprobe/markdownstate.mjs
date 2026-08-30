// Read-only: show canonical, parser, committed, and volatile Markdown sizes per visible assistant row.
// usage: probe markdownstate
import { connectPage, evaluate, done } from './lib/cdp.mjs';

const page = await connectPage();
try {
  const rows = await evaluate(page, `(() => {
    const result = [];
    for (const body of document.querySelectorAll('[data-testid="assistant-message-body"]')) {
      const forensics = body.__aoMarkdownForensics;
      if (!forensics) continue;
      const roots = [];
      for (const root of body.querySelectorAll('.md-committed, .md-volatile')) {
        const streamdown = root.__aoStreamdownForensics;
        roots.push({
          region: root.classList.contains('md-committed') ? 'committed' : 'volatile',
          contentCodeUnits: streamdown?.content?.length ?? null,
          blocks: streamdown?.blocks?.length ?? null,
          domTextCodeUnits: root.textContent?.length ?? 0,
          parseIncompleteMarkdown: streamdown?.parseIncompleteMarkdown ?? null,
          documentParseCalls: streamdown?.documentParseCalls ?? null,
          documentPublications: streamdown?.documentPublications ?? null,
          lastPath: streamdown?.lastPath ?? null,
        });
      }
      result.push({
        paneId: body.closest('[data-pane-id]')?.getAttribute('data-pane-id') ?? null,
        itemId: forensics.itemId,
        streaming: forensics.streaming,
        canonicalCodeUnits: forensics.canonicalSource.length,
        parserCodeUnits: forensics.parserSource.length,
        renderedCodeUnits: forensics.renderedSource.length,
        roots,
      });
    }
    return result;
  })()`);
  console.log(JSON.stringify(rows, null, 2));
} finally {
  await done([page]).catch(() => {});
}
